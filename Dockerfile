FROM golang:1.25.3-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN go test ./... && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /relay .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -g 10001 relay && adduser -D -u 10001 -G relay relay && mkdir /data && chown relay:relay /data
COPY --from=build /relay /usr/local/bin/relay
USER 10001:10001
ENV RELAY_DATA_DIR=/data PORT=8317
EXPOSE 8317
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["relay", "healthcheck"]
ENTRYPOINT ["relay"]
