# forge-hris — multi-stage build, static binary, no cgo.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-hris-api ./cmd/forge-hris-api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-hris-mcp ./cmd/forge-hris-mcp

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget
COPY --from=builder /forge-hris-api /usr/local/bin/forge-hris-api
COPY --from=builder /forge-hris-mcp /usr/local/bin/forge-hris-mcp
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/forge-hris-api"]
# Run the MCP server instead:  docker run ... forge-hris-mcp --addr :8081
# (or leave the image ENTRYPOINT overridden with: forge-hris-mcp --stdio)
