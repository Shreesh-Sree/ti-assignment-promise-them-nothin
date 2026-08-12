FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 go build -tags "${BUILD_TAGS}" -o /out/relayapi ./cmd/relayapi
RUN CGO_ENABLED=0 go build -o /out/harness ./cmd/harness

FROM alpine:3.19
RUN adduser -D -u 10001 relayapi
COPY --from=build /out/relayapi /usr/local/bin/relayapi
COPY --from=build /out/harness /usr/local/bin/harness
USER relayapi
ENTRYPOINT ["/usr/local/bin/relayapi"]
