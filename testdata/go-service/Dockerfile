FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN make build

FROM gcr.io/distroless/static
COPY --from=build /src/bin/orders /orders
ENTRYPOINT ["/orders"]
