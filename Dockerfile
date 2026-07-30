# Build Go binary
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod .
# Glob, not "main.go": naming the one file meant any future source file was
# silently left out of the build.
COPY *.go .
# CGO_ENABLED=0: the builder is musl-based (alpine) but the runtime image is
# glibc-based (debian slim), so a dynamically linked binary would not run there.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o server .

# Runtime image with Python + catt
FROM python:3.12-slim
# PyChromecast is a transitive dependency of catt, but cc_status.py imports it
# directly — declare it so a catt release that drops or renames it fails here
# rather than at runtime.
RUN pip install --no-cache-dir catt PyChromecast

COPY --from=builder /build/server /usr/local/bin/server
COPY scripts/ /usr/local/lib/chromecast/
COPY static/ /static/

VOLUME ["/config"]
EXPOSE 8080
ENV CONFIG_PATH=/config/config.json

CMD ["server"]
