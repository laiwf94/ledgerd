# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledgerd ./cmd/ledgerd && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledgerd /ledgerd
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
ENV LEDGERD_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/ledgerd"]
