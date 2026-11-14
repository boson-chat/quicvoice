# quicvoiced: the voice server. Cross-compiles on the build host (no QEMU for Go).
FROM --platform=$BUILDPLATFORM golang:1.27 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/quicvoiced ./server/cmd

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder --chown=nonroot:nonroot /out/quicvoiced .

ARG VERSION=dev
ENV VERSION=$VERSION ENV=production

# 7700/udp: QUIC voice. 8080/tcp: health + metrics (internal). 8090/tcp: public API.
EXPOSE 7700/udp 8080/tcp 8090/tcp
USER 65532:65532
ENTRYPOINT ["./quicvoiced"]
CMD ["serve"]
