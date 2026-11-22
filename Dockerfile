# Copyright 2025 Mehmet ERGULCU
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/datatransporter ./cmd/datatransporter && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/datatransporter /datatransporter
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DATATRANSPORTER_DATA_DIR=/data
VOLUME /data
EXPOSE 8888
ENTRYPOINT ["/datatransporter"]
