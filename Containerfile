FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/opencode-go-gateway .

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/opencode-go-gateway /opencode-go-gateway

ENV GATEWAY_LISTEN=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/opencode-go-gateway"]
