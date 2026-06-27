.PHONY: fmt test vet run

fmt:
	gofmt -w .

test:
	go test -race ./...

vet:
	go vet ./...

run:
	go run .
