# 更新日志

格式参照 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [v0.1.0] - 2026-11-18

第一个公开版本。

### 网页面板
- 添加、编辑、删除、更新订阅（支持 sing-box JSON，base64 编码的也可以）
- 选择节点或自动选择（urltest），全部测速；切换节点立即生效，不用重启
- 绕过大陆开关：geosite-cn / geoip-cn 规则，规则文件手动更新
- sing-box 内核管理：检查新版本，安装稳定版或测试版（SHA-256 校验，先检查配置再切换，启动失败自动换回），可以回退，也可以改回系统版本
- 代理端口、局域网访问、代理用户名和密码、DNS、测速地址、日志级别
- 查看 sing-box 日志和生成的配置；修改面板密码
- 手机和电脑都能用，跟随系统深色模式

### 可靠性
- 每份配置都先用 `sing-box check` 校验；只剔除不支持的那个节点，其他节点不受影响
- 修改后 sing-box 启动失败，自动回滚到修改前的状态
- sing-box 意外退出后自动重启；面板重启后恢复上次的运行状态和选中的节点
- 订阅默认只手动更新；开了自动更新也会随机推迟；更新时优先走代理；错误信息里不会出现订阅地址

### 终端管理
- `sbpanel` 管理菜单，以及 `status`、`start`、`stop`、`restart`、`enable`、`disable`、`logs`、`passwd`、`listen`、`update`、`uninstall` 子命令
- 一键安装脚本：下载、校验、注册 systemd 服务、开机自启，没有 sing-box 时自动安装官方稳定版
