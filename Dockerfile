# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/freedo ./cmd/freedo
COPY internal ./internal
COPY tado ./tado
COPY cc1101 ./cc1101
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
      -X freedo/internal/buildinfo.Version=${VERSION} \
      -X freedo/internal/buildinfo.Commit=${COMMIT} \
      -X freedo/internal/buildinfo.Date=${DATE}" \
    -o /out/freedo ./cmd/freedo

# alpine instead of a distroless image: busybox wget runs the health check.
FROM alpine:3.24
RUN adduser -D -H -u 10001 freedo && mkdir /data && chown freedo:freedo /data
COPY --from=build /out/freedo /usr/local/bin/freedo
USER 10001:10001
# /data holds freedo.yaml (optional), freedo-state.db and the unhandled log.
WORKDIR /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["freedo"]
# A non-loopback --addr needs an API token: --api-token-file or FREEDO_API_TOKEN.
CMD ["--addr", ":8080"]
