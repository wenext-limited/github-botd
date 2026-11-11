FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/github-botd ./cmd/github-botd
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 65532 botd && adduser -D -u 65532 -G botd botd
COPY --from=build /out/github-botd /usr/local/bin/github-botd
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/github-botd"]
CMD ["-config", "/etc/github-botd/config.json"]
