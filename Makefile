.PHONY: fmt test vet run

fmt:
	gofmt -w .

test:
	go test -race ./...

vet:
	go vet ./...

run:
	go run . --log info --address 0.0.0.0:8080

docker-build:
	docker build -t webbadge:latest .

docker-run:
	docker run --rm -it -p 8080:8080 webbadge:latest --log info --address 0.0.0.0:8080
