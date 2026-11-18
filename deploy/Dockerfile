# A self-hosted accounts + relay in one image.
#
# The build stage carries the whole toolchain; the run stage carries a static
# binary and nothing else. SQLite is the pure-Go driver (modernc.org/sqlite),
# so there is no cgo and no libc to match.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reasonix-selfhost ./cmd/reasonix-selfhost

FROM alpine:3.21
RUN adduser -D -u 10001 reasonix && mkdir -p /data && chown reasonix /data
COPY --from=build /out/reasonix-selfhost /usr/local/bin/reasonix-selfhost
USER reasonix
WORKDIR /data
ENV REASONIX_SELFHOST_ADDR=0.0.0.0:8790 \
    REASONIX_SELFHOST_DATA=/data
VOLUME ["/data"]
EXPOSE 8790
ENTRYPOINT ["/usr/local/bin/reasonix-selfhost"]
