FROM golang:1.24-alpine AS builder

WORKDIR /build

# Prepare the dependencies.
COPY go.mod ./
COPY go.sum ./
RUN go mod download
COPY pkg/gcs/ pkg/gcs/

# Build the tool.
COPY syz-cluster/tools/db-mgmt/*.go syz-cluster/tools/db-mgmt/
COPY syz-cluster/pkg/ syz-cluster/pkg/
RUN go build -o /bin/db-mgmt /build/syz-cluster/tools/db-mgmt

# Create the actual container.
FROM alpine:latest
WORKDIR /app

COPY --from=builder /bin/db-mgmt /bin/db-mgmt

ENTRYPOINT ["/bin/db-mgmt"]
