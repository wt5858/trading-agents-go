.PHONY: wire swagger build vet test fmt check deps-up deps-down up down down-v logs rebuild \
        dev dev-up dev-down dev-logs

# air 的版本钉在这里，而不是 tools.go 里。
#
# tools.go 的做法（wire、swag 都是）会把工具连同它的依赖闭包一起并进主模块的
# go.mod。对 wire 和 swag 这个代价可以忽略，对 air 不行：air v1.67 要求
# go 1.26，`go get` 它会把本模块的 go 指令从 1.23 顶到 1.26，
# 顺带把 wire、cobra、jwt、x/crypto 全部升一遍——一个开发期的文件监听器，
# 不该有能力改动生产二进制的依赖版本。
#
# `go run pkg@version` 在独立的模块上下文里解析，go.mod 与 go.sum 一个字节都不会动。
# v1.61.7 是最后一批仍然声明 go 1.23 的版本，与本项目的工具链正好对齐。
AIR := go run github.com/air-verse/air@v1.61.7

# wire 重新生成依赖注入代码。
#
# 改完 internal/di/injectors/container.go 或任何 provider set 之后必须跑一次，
# 否则跑起来的还是上一次生成的 wire_gen.go——而它照样能编译，
# 只是装配出来的东西和你以为的不一样。
wire:
	go run github.com/google/wire/cmd/wire ./internal/di/injectors

# swagger 从处理器上的注释重新生成 docs/。
#
# 生成物要提交进仓库：internal/server 空导入了 docs 包，没有它整个项目编译不过。
# 让每个 clone 仓库的人先装一遍 swag 才能 go build，代价远高于多提交三个文件。
#
# --parseInternal：swag 默认跳过 internal/ 下的包（那条规则是为依赖准备的），
# 而本项目的处理器全在 internal/ 里——少这个开关，生成出来的是一份空文档。
swagger:
	go run github.com/swaggo/swag/cmd/swag init \
		--generalInfo main.go --output docs --parseInternal --parseDepth 2

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

fmt:
	gofmt -w .

# check 是提交前该跑的那一套。wire 放在最前面：装配没生成对，
# 后面的编译与测试全都是在验证一份过期的接线图。
#
# swagger 同样进这一套，理由不是「顺手生成一下」，而是：接口文档只要不是每次
# 提交前重新生成，它就会安静地停在某个历史版本上——而一份过期的 API 文档
# 比没有文档更贵，因为调用方会信它。
check: wire swagger fmt build vet test

# ---------------------------------------------------------------------------
# 热加载（本地）
# ---------------------------------------------------------------------------

# dev 在宿主机上热加载服务进程，依赖仍走容器（先跑 make deps-up）。
#
# 只有一个进程要热加载：HTTP、消费者、巡检、调度全在 serve 里。
# 这以前是两个 air 用 trap + kill 0 串起来的，合并之后那段编排一并不需要了。
dev:
	$(AIR) -c .air.api.toml

# ---------------------------------------------------------------------------
# 热加载（容器内）
# ---------------------------------------------------------------------------

# 叠加 docker-compose.dev.yml：源码挂进容器，两个服务改跑 air。
# 基础那份不动，因此 make up 起出来的仍然是贴近生产的镜像。
COMPOSE_DEV := docker compose -f docker-compose.yml -f docker-compose.dev.yml

dev-up:
	$(COMPOSE_DEV) up -d --build

dev-down:
	$(COMPOSE_DEV) down

# 容器内的重新编译日志也在这里，改完代码盯着这个看重启有没有成功。
dev-logs:
	$(COMPOSE_DEV) logs -f api

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------

# up 起整套环境（api + 四个依赖）。
# 表结构不需要单独迁移：api 启动时自动执行，并发由数据库具名锁串行化。
up:
	docker compose up -d --build

down:
	docker compose down

# down-v 连数据卷一起删。数据全没，适合「我要一个干净环境」。
down-v:
	docker compose down -v

logs:
	docker compose logs -f api

# rebuild 只重建应用镜像并滚动替换两个服务，不碰依赖容器——
# 改一行业务代码不该让数据库跟着重启。
rebuild:
	docker compose up -d --build --no-deps api

# ---------------------------------------------------------------------------
# 本地（不走容器跑应用，只起依赖）
# ---------------------------------------------------------------------------

# 本地依赖。pkg/mq 的集成测试需要 RabbitMQ，没有时自动跳过。
deps-up:
	docker run -d --rm --name ta-mysql -p 3306:3306 \
		-e MYSQL_ALLOW_EMPTY_PASSWORD=yes -e MYSQL_DATABASE=trading_agents mysql:8.0
	docker run -d --rm --name ta-mongo -p 27017:27017 mongo:7
	docker run -d --rm --name ta-redis -p 6379:6379 redis:7-alpine
	docker run -d --rm --name ta-rabbit -p 5672:5672 rabbitmq:3.13-alpine

deps-down:
	-docker stop ta-mysql ta-mongo ta-redis ta-rabbit
