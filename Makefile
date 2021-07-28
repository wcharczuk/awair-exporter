run:
	@BIND_ADDR=":8080" go run main.go

docker-build:
	@docker build . -t localhost:32000/awair-exporter:latest
	@docker push localhost:32000/awair-exporter:latest

