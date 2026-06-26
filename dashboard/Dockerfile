FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOAMD64=v2 go build -o /dashboard ./cmd/dashboard

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /dashboard /dashboard
USER 65532:65532
ENTRYPOINT ["/dashboard"]
CMD ["-config", "/config/config.yaml"]
