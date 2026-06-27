# syntax=docker/dockerfile:1

FROM golang:1.26.4 AS builder

ARG tag="latest"
ARG commit="-"

# Set destination for COPY
# and copy the source code
WORKDIR /go/src/app
COPY . .

# Download and verify Go modules
RUN go mod download
RUN go mod verify
# Run Go tests and checks
RUN go vet -v
RUN go test -v

# Build
RUN CGO_ENABLED=0 go build -v -ldflags "-X main.Version=${tag} -X main.Commit=${commit}" -o /go/bin/webbadge .

# Copy the built executable into a new base image and run it
FROM gcr.io/distroless/static-debian12:nonroot
HEALTHCHECK NONE
WORKDIR /app
USER nonroot
COPY --from=builder /go/bin/webbadge .

ENTRYPOINT ["/app/webbadge"]
CMD ["--help"]
