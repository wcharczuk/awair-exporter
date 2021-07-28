package main

import (
	"context"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

// these may change based on DHCP settings.
var awairSensors = map[string]string{
	"Bedroom":     "192.168.53.1",
	"Living Room": "192.168.53.235",
}

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

	flag.Parse()

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
	var sensors []string
	var resultsMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(len(awairSensors))
	errors := make(chan error, len(awairSensors))
	for sensor, host := range awairSensors {
		go func(s, h string) {
			defer wg.Done()
			data, err := getAwairData(r.Context(), h)
			if err != nil {
				errors <- err
				return
			}
			resultsMu.Lock()
			sensorData[s] = data
			sensors = append(sensors, s)
			resultsMu.Unlock()
		}(sensor, host)
	}
	wg.Wait()

	if len(errors) > 0 {
		http.Error(rw, fmt.Sprintf("error fetching data; %v", <-errors), http.StatusInternalServerError)
		return
	}

	sort.Strings(sensors)

	rw.Header().Add("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)

	for _, sensor := range sensors {
		data, ok := sensorData[sensor]
		if !ok {
			continue
		}

		fmt.Fprintf(rw, "awair_score{sensor=%q} %f\n", sensor, data.Score)
		fmt.Fprintf(rw, "awair_dew_point{sensor=%q} %f\n", sensor, data.DewPoint)
		fmt.Fprintf(rw, "awair_temp{sensor=%q} %f\n", sensor, data.Temp)
		fmt.Fprintf(rw, "awair_humid{sensor=%q} %f\n", sensor, data.Humid)
		fmt.Fprintf(rw, "awair_co2{sensor=%q} %f\n", sensor, data.CO2)
		fmt.Fprintf(rw, "awair_voc{sensor=%q} %f\n", sensor, data.VOC)
		fmt.Fprintf(rw, "awair_voc_baseline{sensor=%q} %f\n", sensor, data.VOCBaseline)
		fmt.Fprintf(rw, "awair_voc_h2_raw{sensor=%q} %f\n", sensor, data.VOCH2Raw)
		fmt.Fprintf(rw, "awair_voc_ethanol_raw{sensor=%q} %f\n", sensor, data.VOCEthanolRaw)
		fmt.Fprintf(rw, "awair_pm25{sensor=%q} %f\n", sensor, data.PM25)
		fmt.Fprintf(rw, "awair_pm10_est{sensor=%q} %f\n", sensor, data.PM10Est)
	}
	return
}

// Awair is the latest awair data from a sensor.
type Awair struct {
	Timestamp     time.Time `json:"timestamp"`
	Score         float64   `json:"score"`
	DewPoint      float64   `json:"dew_point"`
	Temp          float64   `json:"temp"`
	Humid         float64   `json:"humid"`
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
	res, err = http.DefaultClient.Do(req)
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
