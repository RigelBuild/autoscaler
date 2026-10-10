FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
# One compile at a time: the ec2 + client-go compiles OOM a 4 GiB pool agent in parallel.
ENV GOFLAGS=-p=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    make build

FROM --platform=$BUILDPLATFORM scratch
ENV GODEBUG=netdns=go

# copy certs from build image
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# copy agent binary
COPY --from=build /src/dist/woodpecker-autoscaler /bin/

ENTRYPOINT ["/bin/woodpecker-autoscaler"]
