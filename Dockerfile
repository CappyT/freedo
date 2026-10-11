# syntax=docker/dockerfile:1

# The Node line of the dashboard build. This is the one place that names it: the CI job of
# the dashboard reads this line. SvelteKit 3 needs Node 22.17 or later.
ARG NODE_VERSION=24

# The dashboard stage builds static files, so one build serves every platform. The
# layout under /src follows the repository: the app imports ../assets/brand.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS dashboard
WORKDIR /src/dashboard
COPY dashboard/package.json dashboard/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY assets/brand /src/assets/brand
COPY dashboard ./
RUN npm run build

# The build stage runs on the build host and cross-compiles: the daemon is pure
# Go (CGO_ENABLED=0, the sqlite driver is modernc), so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/freedo ./cmd/freedo
COPY internal ./internal
COPY tado ./tado
COPY cc1101 ./cc1101
# The daemon embeds the dashboard from internal/web/dist. A missing build fails here.
COPY --from=dashboard /src/dashboard/build ./internal/web/dist
RUN test -f internal/web/dist/index.html
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w \
      -X freedo/internal/buildinfo.Version=${VERSION} \
      -X freedo/internal/buildinfo.Commit=${COMMIT} \
      -X freedo/internal/buildinfo.Date=${DATE}" \
    -o /out/freedo ./cmd/freedo

# The Home Assistant add-on image: build it with --target addon. The Supervisor
# starts the CMD as root and reads io.hass.version to match the image to the
# version in addon/config.yaml. No io.hass.arch: one manifest serves both
# platforms and the Supervisor takes the platform from the image itself.
FROM alpine:3.24 AS addon
RUN apk add --no-cache jq curl
COPY --from=build /out/freedo /usr/local/bin/freedo
COPY --chmod=755 addon/run.sh /run.sh
ARG VERSION=dev
LABEL io.hass.type="app" io.hass.version="${VERSION}"
CMD ["/run.sh"]

# The standalone daemon image. It is the last stage, so a plain docker build
# produces it.
# alpine instead of a distroless image: busybox wget runs the health check.
FROM alpine:3.24 AS daemon
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
