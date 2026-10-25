package main

import (
	"context"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// awairSensors maps a sensor name to its host, from `--sensor name=host` flags.
// Prefer the sensor's mDNS name (e.g. AWAIR-ELEM-142484.local) to its IP; IPs
// may change based on DHCP settings.
var awairSensors = sensorFlags{}

type sensorFlags map[string]string

func (sf sensorFlags) String() string {
	var pairs []string
	for name, host := range sf {
		pairs = append(pairs, name+"="+host)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (sf sensorFlags) Set(value string) error {
	name, host, ok := strings.Cut(value, "=")
	if !ok || name == "" || host == "" {
		return fmt.Errorf("expected name=host, got %q", value)
	}
	sf[name] = host
	return nil
}

// sensorTimeout bounds each sensor fetch, so a sensor that's down doesn't hold up the others.
const sensorTimeout = 5 * time.Second

// sensorClient connects over IPv4 only. Sensors have no IPv6 address, and on
// macOS an mDNS (.local) lookup that also asks for one waits 5 s for it.
var sensorClient = &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
	},
}}

var (
	flagBindAddr    = flag.String("bind-addr", "", "The http server bind address (e.g. `127.0.0.1:8081`)")
	flagLogHide     = flag.Bool("hide-log", false, "If we should suppress all log output")
	flagLogHideDate = flag.Bool("hide-log-date", false, "If we should omit the date from log output")
	flagLogHideFile = flag.Bool("hide-log-file", false, "If we should omit the file from log output")
)

var (
	varAwairSensorCount                 = new(expvar.Int)
	varHTTPRequestCount                 = new(expvar.Int)
	varHTTPRequestDeadlineExceededCount = new(expvar.Int)
	varHTTPRequestCanceledCount         = new(expvar.Int)
	varHTTPRequestErrorCount            = new(expvar.Int)
	varHTTPRequestElapsedLast           = new(expvar.Float)
	varHTTPRequestElapsedAvg            = new(expvar.Float)
	varHTTPRequestElapsedP95            = new(expvar.Float)
	varHTTPRequestElapsedMin            = new(expvar.Float)
	varHTTPRequestElapsedMax            = new(expvar.Float)
)

func init() {
	expvar.Publish("awair.sensor.count", varAwairSensorCount)
	expvar.Publish("http.request.count", varHTTPRequestCount)
	expvar.Publish("http.request.deadline_exceeded.count", varHTTPRequestDeadlineExceededCount)
	expvar.Publish("http.request.canceled.count", varHTTPRequestCanceledCount)
	expvar.Publish("http.request.error.count", varHTTPRequestErrorCount)
	expvar.Publish("http.request.elapsed.last", varHTTPRequestElapsedLast)
	expvar.Publish("http.request.elapsed.avg", varHTTPRequestElapsedAvg)
	expvar.Publish("http.request.elapsed.p95", varHTTPRequestElapsedP95)
	expvar.Publish("http.request.elapsed.min", varHTTPRequestElapsedMin)
	expvar.Publish("http.request.elapsed.max", varHTTPRequestElapsedMax)

	flag.Var(awairSensors, "sensor", "A sensor to poll, as `name=host` (repeatable)")
	flag.Parse()
	varAwairSensorCount.Set(int64(len(awairSensors)))

	if *flagLogHide {
		log.SetOutput(io.Discard)
	} else {
		var logFlags int
		if !*flagLogHideDate {
			logFlags |= log.Ldate
			logFlags |= log.Lmicroseconds
			logFlags |= log.LUTC
		}
		if !*flagLogHideFile {
			logFlags |= log.Lshortfile
		}
		log.SetFlags(logFlags)
	}
}

func bindAddr() string {
	if *flagBindAddr != "" {
		return *flagBindAddr
	}
	if value := os.Getenv("BIND_ADDR"); value != "" {
		return value
	}
	return "127.0.0.1:8080"
}

func main() {
	http.HandleFunc("/", getSensorData)
	http.HandleFunc("/sensors", getSensors)
	http.HandleFunc("/prometheus", getSensorData)
	http.HandleFunc("/metrics", getSensorData)

	server := &http.Server{
		Addr:    bindAddr(),
		Handler: loggedHandler{http.DefaultServeMux},
	}

	log.Println("http server listening on:", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

type loggedHandler struct {
	Server http.Handler
}

func (lh loggedHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	varHTTPRequestCount.Add(1)

	start := time.Now()
	irw := &responseWriter{ResponseWriter: rw}

	defer func() {
		if irw.statusCode != http.StatusOK {
			varHTTPRequestErrorCount.Add(1)
		}
		contextErr := r.Context().Err()
		if contextErr == context.DeadlineExceeded {
			varHTTPRequestDeadlineExceededCount.Add(1)
		} else if contextErr == context.Canceled {
			varHTTPRequestCanceledCount.Add(1)
		}

		varHTTPRequestElapsedLast.Set(float64(time.Since(start)) / float64(time.Millisecond))
		log.Println(fmt.Sprintf("%s %d %s %v", r.URL.Path, irw.statusCode, formatContentLength(irw.contentLength), time.Since(start)))
	}()
	lh.Server.ServeHTTP(irw, r)
}

func getSensors(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set(http.CanonicalHeaderKey("Content-Type"), "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	json.NewEncoder(rw).Encode(awairSensors)
}

func getSensorData(rw http.ResponseWriter, r *http.Request) {
	sensorData := map[string]*Awair{}
	var resultsMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(len(awairSensors))
	for sensor, host := range awairSensors {
		go func(s, h string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), sensorTimeout)
			defer cancel()
			data, err := getAwairData(ctx, h)
			if err != nil {
				// Reported as awair_up 0; getJSON has logged the error.
				return
			}
			resultsMu.Lock()
			sensorData[s] = data
			resultsMu.Unlock()
		}(sensor, host)
	}
	wg.Wait()

	var sensors []string
	for sensor := range awairSensors {
		sensors = append(sensors, sensor)
	}
	sort.Strings(sensors)

	rw.Header().Add("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	rw.WriteHeader(http.StatusOK)

	fmt.Fprintln(rw, "# HELP awair_up Whether the sensor answered (1) or not (0).")
	fmt.Fprintln(rw, "# TYPE awair_up gauge")
	for _, sensor := range sensors {
		up := 0
		if _, ok := sensorData[sensor]; ok {
			up = 1
		}
		fmt.Fprintf(rw, "awair_up{sensor=%q} %d\n", sensor, up)
	}

	// The text format wants each metric's samples together, so write a
	// metric at a time across sensors.
	for _, m := range awairMetrics {
		fmt.Fprintf(rw, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(rw, "# TYPE %s gauge\n", m.name)
		for _, sensor := range sensors {
			if data, ok := sensorData[sensor]; ok {
				fmt.Fprintf(rw, "%s{sensor=%q} %f\n", m.name, sensor, m.value(data))
			}
		}
	}
}

var awairMetrics = []struct {
	name, help string
	value      func(*Awair) float64
}{
	{"awair_score", "Awair score, 0-100.", func(a *Awair) float64 { return a.Score }},
	{"awair_dew_point", "Dew point, in degrees Celsius.", func(a *Awair) float64 { return a.DewPoint }},
	{"awair_temp", "Temperature, in degrees Celsius.", func(a *Awair) float64 { return a.Temp }},
	{"awair_humid", "Relative humidity, in percent.", func(a *Awair) float64 { return a.Humid }},
	{"awair_abs_humid", "Absolute humidity, in grams per cubic meter.", func(a *Awair) float64 { return a.AbsHumid }},
	{"awair_co2", "Carbon dioxide, in ppm.", func(a *Awair) float64 { return a.CO2 }},
	{"awair_voc", "Total volatile organic compounds, in ppb.", func(a *Awair) float64 { return a.VOC }},
	{"awair_voc_baseline", "VOC sensor baseline.", func(a *Awair) float64 { return a.VOCBaseline }},
	{"awair_voc_h2_raw", "VOC sensor raw hydrogen signal.", func(a *Awair) float64 { return a.VOCH2Raw }},
	{"awair_voc_ethanol_raw", "VOC sensor raw ethanol signal.", func(a *Awair) float64 { return a.VOCEthanolRaw }},
	{"awair_pm25", "Fine particulate matter (PM2.5), in micrograms per cubic meter.", func(a *Awair) float64 { return a.PM25 }},
	{"awair_pm10_est", "Estimated coarse particulate matter (PM10), in micrograms per cubic meter.", func(a *Awair) float64 { return a.PM10Est }},
}

// Awair is the latest awair data from a sensor.
type Awair struct {
	Timestamp     time.Time `json:"timestamp"`
	Score         float64   `json:"score"`
	DewPoint      float64   `json:"dew_point"`
	Temp          float64   `json:"temp"`
	Humid         float64   `json:"humid"`
	AbsHumid      float64   `json:"abs_humid"`
	CO2           float64   `json:"co2"`
	VOC           float64   `json:"voc"`
	VOCBaseline   float64   `json:"voc_baseline"`
	VOCH2Raw      float64   `json:"voc_h2_raw"`
	VOCEthanolRaw float64   `json:"voc_ethanol_raw"`
	PM25          float64   `json:"pm25"`
	PM10Est       float64   `json:"pm10_est"`
}

func getAwairData(ctx context.Context, host string) (*Awair, error) {
	const path = "/air-data/latest"
	req := http.Request{
		Method: "GET",
		URL: &url.URL{
			Scheme: "http",
			Host:   host,
			Path:   path,
		},
	}
	var data Awair
	err := getJSON(ctx, &req, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

func getJSON(ctx context.Context, req *http.Request, output interface{}) (err error) {
	started := time.Now()
	var statusCode int
	defer func() {
		if err != nil {
			log.Println(fmt.Sprintf("GET %s %d %v %v", req.URL.String(), statusCode, time.Since(started), err))
		} else {
			log.Println(fmt.Sprintf("GET %s %d %v", req.URL.String(), statusCode, time.Since(started)))
		}
	}()

	req = req.WithContext(ctx)
	var res *http.Response
	res, err = sensorClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if statusCode = res.StatusCode; statusCode < http.StatusOK || statusCode > 299 {
		return fmt.Errorf("non-200 returned from remote")
	}
	err = json.NewDecoder(res.Body).Decode(output)
	return
}

// net/http helpers

const (
	sizeofByte = 1 << (10 * iota)
	sizeofKilobyte
	sizeofMegabyte
	sizeofGigabyte
)

func formatContentLength(contentLength uint64) string {
	if contentLength >= sizeofGigabyte {
		return fmt.Sprintf("%0.2fgB", float64(contentLength)/float64(sizeofGigabyte))
	} else if contentLength >= sizeofMegabyte {
		return fmt.Sprintf("%0.2fmB", float64(contentLength)/float64(sizeofMegabyte))
	} else if contentLength >= sizeofKilobyte {
		return fmt.Sprintf("%0.2fkB", float64(contentLength)/float64(sizeofKilobyte))
	}
	return fmt.Sprintf("%dB", contentLength)
}

type responseWriter struct {
	http.ResponseWriter

	statusCode    int
	contentLength uint64
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *responseWriter) Write(data []byte) (n int, err error) {
	n, err = rw.ResponseWriter.Write(data)
	rw.contentLength += uint64(n)
	return
}
