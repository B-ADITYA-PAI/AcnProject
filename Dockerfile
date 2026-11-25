FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.19
RUN apk add --no-cache iproute2 ca-certificates
COPY --from=build /out/ /usr/local/bin/
COPY config.yaml /config.yaml
