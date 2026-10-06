FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o /out/libkona.so ./cmd/module
RUN CGO_ENABLED=0 go build -trimpath -o /out/kona-data ./cmd/dataserver
RUN CGO_ENABLED=0 go build -trimpath -o /out/kona-bootstrap ./cmd/bootstrap
FROM envoyproxy/envoy:v1.38.0 AS proxy
COPY --from=build /out/libkona.so /usr/local/lib/libkona.so
FROM gcr.io/distroless/static-debian12:nonroot AS data
COPY --from=build /out/kona-data /kona-data
COPY --from=build /out/kona-bootstrap /kona-bootstrap
ENTRYPOINT ["/kona-data"]
