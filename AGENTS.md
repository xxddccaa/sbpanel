# AGENTS.md — sbpanel 项目入口

每次开始新会话，先按顺序读取项目真相，再动代码：
1. `.agent-forge/STATE.md` — 当前阶段、在办任务与最近有效决策
2. `.agent-forge/constitution.md` — 本项目工程原则与不可绕过质量底线
3. `.agent-forge/specs/architecture.md` 与本次改动涉及的 `.agent-forge/specs/domains/*.md` / `data-model.md`
4. `.agent-forge/decisions/` 中顶部标注 `Status: Active` 的记录

## 读取过滤规则
- 跳过 `.agent-forge/changes/archive/`（历史归档，除非用户点名考古）。
- 跳过顶部标注 `Status: Superseded by ...` 的决策文档。
- `.agent-forge/specs/` 永远表示当前现状，更新时原地重写，不写“某日新增/修改”式日记。

## 改动分级约定（L0 / L1 / L2）
- **L0（单点 Bug 速修 / 局部微调）**：不改模块边界和数据格式；直接定位→补复现测试→修复→验证 `make lint`、`make test`。
- **L1（常规功能增量 / 局部重构）**：涉及新 API、数据结构或跨文件协作；先在 `.agent-forge/changes/active/` 写轻量变更方案，完工后同步回 `.agent-forge/specs/` 并归档变更文档。
- **L2（架构级演进 / 推倒重做）**：改变进程模型、目录结构、存储方案或核心安全边界；先完成需求澄清与架构评审并新增 ADR，用户确认后再写代码。

## 构建与校验命令
- `make test`：运行全部单元测试（`go test ./...`）
- `make lint`：检查 `gofmt`、`go vet ./...` 与 `node --check web/app.js`
- `make build`：构建当前平台无 CGO 静态二进制 `./sbpanel`
- `make dist`：交叉编译 Linux `amd64/arm64/armv7/386` 发布包与 `sha256sums.txt`

## 不可绕过底线（摘要，详见 `.agent-forge/constitution.md`）
- 保持 Go 纯标准库（零第三方依赖）与原生前端零构建工具链。
- 所有外部下载产物（sing-box、sbpanel、规则集）必须通过完整性校验或 `sing-box check` 验证。
- 禁止新增压制注释、空 `catch {}`、假桩、删改/跳过测试或降级检查阈值。
- 禁止向 `docs/` 或根目录溢写未指定的说明文档；源码注释默认英文，只写 `Why`。
