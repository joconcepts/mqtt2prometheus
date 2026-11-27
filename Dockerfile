# Build on the runner's native platform and cross-compile, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /mqtt2prometheus .

FROM scratch
COPY --from=build /mqtt2prometheus /mqtt2prometheus
EXPOSE 8000
ENTRYPOINT ["/mqtt2prometheus"]
