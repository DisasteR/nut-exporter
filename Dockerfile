# Copyright 2026 The nut-exporter Authors
# SPDX-License-Identifier: Apache-2.0

# Base images pinned by digest; bump both tag and digest together.
FROM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /nut-exporter ./cmd/nut-exporter
# Licence and notice files of everything compiled in: this project, the Go standard library and
# each module (scripts/collect-licenses.sh fails on a module without one).
RUN mkdir -p /licenses/nut-exporter /licenses/go && cp LICENSE NOTICE /licenses/nut-exporter/ \
 && cp "$(go env GOROOT)/LICENSE" /licenses/go/ \
 && sh scripts/collect-licenses.sh ./cmd/nut-exporter /licenses

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /nut-exporter /nut-exporter
COPY --from=build /licenses /licenses
USER nonroot:nonroot
EXPOSE 9199
ENTRYPOINT ["/nut-exporter"]
