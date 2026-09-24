# syntax=docker/dockerfile:1

# ===========================================================================
# 前端构建阶段
# ===========================================================================
#
# 必须排在 Go 构建阶段前面：那边的 go:embed 要把 web/dist 收进二进制，
# 没有产物就直接编译失败。
FROM node:22-alpine AS web-builder

WORKDIR /web

# 同 Go 那边的道理：先只拷依赖清单。package.json / package-lock.json 没变时
# 这一层命中缓存，改一行前端代码不必重装几百个包（那是分钟级的差别）。
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci

# 类型生成的产物不进版本库，但它是 tsc 的输入——拷 swagger.json 进来现生成一份。
# 不这么做的话，构建到 tsc 那一步会因为找不到 src/types/api.generated.ts 而失败。
COPY docs/swagger.json /docs/swagger.json
COPY web/ ./
RUN npx swagger2openapi /docs/swagger.json --outfile .openapi3.json \
    && npx openapi-typescript .openapi3.json -o src/types/api.generated.ts \
    && npm run build

# ===========================================================================
# 构建阶段
# ===========================================================================
FROM golang:1.25-alpine AS builder

WORKDIR /src

# 先只拷依赖清单再下载：只要 go.mod/go.sum 没变，这一层就能命中缓存，
# 改一行业务代码不必重新拉一遍依赖。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# 前端产物覆盖到 web/dist。
#
# 顺序不能反：上面的 COPY . . 会把宿主机 web/ 目录原样带进来，其中的 dist
# 要么只有一个占位文件（仓库里提交的那个），要么是开发机上某次本地构建的残留。
# 两种情况都不该进镜像——前者让页面全是 404，后者更坏，是一份和当前源码
# 对不上的旧前端，而且没有任何症状提示你它是旧的。
COPY --from=web-builder /web/dist ./web/dist

# CGO_ENABLED=0 产出静态二进制，才能放进一个没有 libc 的运行镜像。
# -trimpath 去掉构建机的绝对路径，让同样的源码在不同机器上产出一致的二进制。
# -s -w 去掉符号表与调试信息，镜像小十几兆。
#
# 迁移脚本通过 go:embed 编进二进制，因此运行镜像里不需要单独拷 *.sql——
# 迁移脚本和跑它的代码永远来自同一次构建，不可能出现版本对不上。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/trading-agents .

# ===========================================================================
# 开发阶段（热加载）
# ===========================================================================
#
# 只有 docker-compose.dev.yml 会用 --target dev 选到它。
#
# 它刻意插在 builder 与运行阶段**之间**：不带 --target 构建时 Docker 取的是
# 最后一个阶段，把 dev 放到文件末尾会让 `make up` 悄悄起一个带 Go 工具链的
# 开发镜像——照样能跑，但和生产跑的已经是两个东西了。
FROM golang:1.25-alpine AS dev

# ca-certificates 用来拉模块和调上游；tzdata 的理由与运行阶段相同。
RUN apk add --no-cache ca-certificates tzdata

# air 预装进镜像。留到容器启动时再 go run 的话，每次 up 都要重新解析并编译
# 它的依赖闭包——几十秒，而且要求联网。
# 版本与 Makefile 里的 AIR 变量保持一致，改一处记得改另一处。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/air-verse/air@v1.61.7

WORKDIR /src
ENV TZ=Asia/Shanghai
EXPOSE 8080

# 跑哪一份配置由 compose 的 command 决定（api 一份、worker 一份）。
CMD ["air", "-c", ".air.api.toml"]

# ===========================================================================
# 运行阶段
# ===========================================================================
FROM alpine:3.20

# ca-certificates：要走 HTTPS 调大模型与行情接口，没有根证书全部握手失败。
# tzdata：cron 表达式按本地时区解释，容器默认只有 UTC，
#         「每天 09:30 开盘同步」会在实际的 17:30 才跑。
RUN apk add --no-cache ca-certificates tzdata

# 以非 root 运行。这个进程不需要任何特权：它只连数据库、发 HTTP 请求、监听一个高位端口。
RUN addgroup -S app && adduser -S -G app app

WORKDIR /app

COPY --from=builder /out/trading-agents /app/trading-agents

# 镜像里没有配置文件。全部配置来自环境变量（TA_ 前缀）加上内置默认值，
# 见 config/setDefaults 与 internal/helpers/constants/mq.go。
#
# 不带配置文件的好处不只是少一个文件：一份烤进镜像的基线配置会变成
# 「到底是哪个值生效了」的第二个答案来源，而排查配置问题时，
# 有两个来源就等于没有来源。

USER app

# 时区默认跟随宿主/编排层的 TZ；没设时用上海时间，与 A 股交易时段一致。
ENV TZ=Asia/Shanghai

EXPOSE 8080

# 健康检查打在业务端点上而不是端口上：端口通了只说明进程活着，
# 而 /healthz 走的是完整的路由与响应封装链路。
#
# 端口跟随 TA_HTTP_PORT，不写死 8080：那是受支持的覆盖项（见 .env.example），
# 而 .env 会被 compose 整体注入容器。写死的话，一旦有人改了端口，进程正常服务、
# 健康检查却永远探不到，容器被标成 unhealthy——编排层据此反复重启一个健康的服务。
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${TA_HTTP_PORT:-8080}/healthz" || exit 1

ENTRYPOINT ["/app/trading-agents"]
# 默认起 HTTP 服务；worker 用 `command: ["worker"]` 覆盖。
CMD ["serve"]
