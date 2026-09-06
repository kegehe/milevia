FROM golang:1.26-alpine AS build
WORKDIR /src
COPY apps/cloud-control/go.mod ./
RUN go mod download
COPY apps/cloud-control ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/cloud-control ./cmd/cloud-control

FROM alpine:3.22
RUN adduser -D -H -u 10001 milevia
USER milevia
COPY --from=build /out/cloud-control /usr/local/bin/cloud-control
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/cloud-control"]
