# 列出可用命令。
default:
    @just --list

# 编译当前系统的服务端和 CLI，输出到 dist/。
build:
    go build -trimpath -ldflags="-s -w" -o dist/ ./cmd/ws-downloader ./cmd/cli

# 在本机交叉编译 Linux 静态程序，输出到 dist/linux-<arch>/。
build-linux arch="amd64":
    CGO_ENABLED=0 GOOS=linux GOARCH={{ quote(arch) }} go build -trimpath -ldflags="-s -w" -o {{ quote("dist/linux-" + arch + "/") }} ./cmd/ws-downloader ./cmd/cli

# 先在本机编译，再将程序复制进 Docker 镜像；架构可选 amd64、arm64 等。
docker image="wingao/ws-file-share:latest" arch="amd64": (build-linux arch)
    docker build --platform {{ quote("linux/" + arch) }} -t {{ quote(image) }} .
