# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
# go.sum is generated here if it is not committed yet (see `make tidy`).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -o /out/mockprovider ./cmd/mockprovider

FROM alpine:3.20 AS api
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/api /usr/local/bin/api
EXPOSE 8080
ENTRYPOINT ["api"]

FROM alpine:3.20 AS mockprovider
COPY --from=build /out/mockprovider /usr/local/bin/mockprovider
EXPOSE 9090
ENTRYPOINT ["mockprovider"]
