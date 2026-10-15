# forge-hris — multi-stage build, static binary, no cgo.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-hris-api ./cmd/forge-hris-api

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget
COPY --from=builder /forge-hris-api /usr/local/bin/forge-hris-api
EXPOSE 8080
ENTRYPOINT ["forge-hris-api"]
