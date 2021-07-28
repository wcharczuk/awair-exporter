FROM golang:1.16-alpine

WORKDIR "/go/src"

ADD go.mod /go/src/go.mod
ADD main.go /go/src/main.go
RUN go build -o /go/bin/awair-exporter main.go 

ENTRYPOINT /go/bin/awair-exporter
EXPOSE 8080
