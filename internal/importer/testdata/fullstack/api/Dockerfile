FROM golang:1.25 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips-dev ca-certificates
WORKDIR /app
COPY . .
