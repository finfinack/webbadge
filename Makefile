.PHONY: fmt test vet run

fmt:
	gofmt -w .

test:
	go test -race ./...

vet:
	go vet ./...

run:
	go run .

docker-build:
	docker build -t webbadge:latest .

docker-run:
	docker run -p 8080:8080 webbadge:latest
