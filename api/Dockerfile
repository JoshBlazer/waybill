# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
# No separate `go mod download`: it would fetch every module in go.mod,
# including the large dependency trees of the pinned codegen tools (sqlc).
# `go build` fetches only what the binary links.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/waybill ./cmd/waybill

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/waybill /usr/local/bin/waybill
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/waybill"]
CMD ["api"]
