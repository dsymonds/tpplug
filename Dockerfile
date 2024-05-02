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
RUN cd cmd/solarctrl && go build -o solarctrl -v

# -----

FROM alpine:3.18 AS runtime

COPY --from=build /go/src/tpplug/tpplug /
COPY --from=build /go/src/tpplug/cmd/solarctrl/solarctrl /
ENTRYPOINT ["/tpplug"]
