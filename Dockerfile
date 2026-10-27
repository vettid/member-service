# syntax=docker.io/docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# VettID member API: small, non-root, static image (MEMBER-SERVICE-PLAN §9).
#   docker build -t member-api .
#   docker buildx build --platform linux/amd64,linux/arm64 -t member-api .
# Binary path is stable: /member-api.

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/member-api ./api/cmd/member-api \
 && test -s /etc/ssl/certs/ca-certificates.crt

# scratch rather than distroless: every base image comes from VettID's
# mirror in CI, and the mirror carries the Go builder only. The binary is
# static; it needs the CA bundle (the service vault, AWS APIs) and nothing else.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/member-api /member-api
USER 65532:65532
ENV MEMBER_API_ADDR=:8081
EXPOSE 8081
ENTRYPOINT ["/member-api"]
CMD ["serve"]
