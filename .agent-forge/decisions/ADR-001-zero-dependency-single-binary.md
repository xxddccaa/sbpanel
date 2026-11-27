# ADR-001: Zero-Dependency Single Binary with Embedded Vanilla Frontend

- **Status**: Active
- **Date**: 2026-11-18 (inferred from v0.1.0 baseline)

## 背景
`sbpanel` 目标运行在无桌面、资源受限或网络受限的 Linux 服务器、容器与开发机上，需要一行命令即可安装、跨 4 种 Linux 架构发布，且维护成本低。

## 决策
1. Go 后端（Go 1.22+）严禁引入任何第三方依赖，`go.mod` 保持纯标准库，使用 `CGO_ENABLED=0` 编译静态单文件。
2. 前端（`web/index.html`、`web/style.css`、`web/app.js`）采用原生 HTML/CSS/JS，通过 `//go:embed web` 打包进二进制，不引入 npm / bundler 构建链。

## 影响与约束
- **收益**：部署零运行时依赖，CI 无需依赖缓存，构建快、供应链攻击面极小。
- **代价与后续约束**：未来实现 `ROADMAP.md` 中的 Clash YAML 订阅解析或更复杂 UI 时，需自行编写轻量解析器（或在确需引入外部库时通过新 ADR 显式变更本决策）。
