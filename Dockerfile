# Build: docker build -t quotad .
# Run:   docker run -p 8080:8080 quotad
FROM golang:1.24-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /quotad ./cmd/quotad

# distroless/static has CA certificates (for https upstreams) and a nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /quotad /quotad
# Inside a container quotad must listen on all interfaces to be reachable.
ENV QUOTAD_LISTEN=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/quotad"]
