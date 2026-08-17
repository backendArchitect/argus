# Multi-stage: build a static binary, ship it on a distroless base.
#
# The runtime image is nonroot and carries no shell. That is not only hygiene —
# argus is pointed at production clusters, so the smallest possible attack
# surface around a credential-bearing process is the whole point.
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/backendArchitect/argus/internal/tools.injected=${VERSION}" \
      -o /out/argus .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/argus /argus
USER nonroot:nonroot
# stdio transport by default: the container is meant to be exec'd by an MCP host.
ENTRYPOINT ["/argus"]
CMD ["serve"]
