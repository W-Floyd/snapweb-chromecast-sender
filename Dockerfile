# Build Go binary
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod .
COPY main.go .
RUN go build -ldflags="-s -w" -o server .

# Runtime image with Python + catt
FROM python:3.12-slim
RUN pip install --no-cache-dir catt

COPY --from=builder /build/server /usr/local/bin/server
COPY static/ /static/

VOLUME ["/config"]
EXPOSE 8080
ENV CONFIG_PATH=/config/config.json

CMD ["server"]
