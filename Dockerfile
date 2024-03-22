# Build this with
#   docker build -t dsymonds/tpplug .

FROM golang:1.22-alpine AS build

WORKDIR /go/src/tpplug
COPY go.mod go.sum ./
RUN go mod download
RUN go build -v \
  github.com/prometheus/client_golang/prometheus

COPY . .
RUN go build -o tpplug -v

# -----

FROM alpine:3.18

COPY --from=build /go/src/tpplug/tpplug /
ENTRYPOINT ["/tpplug"]
