FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -buildvcs=false -trimpath -ldflags="-s -w" -o /mailspot ./cmd/mailspot

FROM alpine:3.23
RUN addgroup -g 10001 mailspot && adduser -D -u 10001 -G mailspot mailspot && mkdir /data && chown mailspot:mailspot /data
COPY --from=build /mailspot /usr/local/bin/mailspot
USER mailspot
ENV SMTP_ADDR=0.0.0.0:1025 HTTP_ADDR=0.0.0.0:8080 DB_PATH=/data/mailspot.db
EXPOSE 1025 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/mailspot"]
