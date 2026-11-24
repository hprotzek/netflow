FROM golang:1.27 AS build
# the asm symlink lets clang -target bpf find the arch headers that linux/types.h needs
RUN apt-get update && apt-get install -y --no-install-recommends clang llvm libbpf-dev \
    && ln -s /usr/include/$(uname -m)-linux-gnu/asm /usr/include/asm
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./... && go vet ./... && go test ./... && CGO_ENABLED=0 go build -o /netflow .

FROM gcr.io/distroless/static
COPY --from=build /netflow /netflow
ENTRYPOINT ["/netflow"]
