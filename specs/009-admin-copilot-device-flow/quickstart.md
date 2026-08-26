# Quickstart: 管理页 Copilot Device Flow 授权

## 自动验证

```bash
task check
task test-race
```

预期：格式、vet、全部测试与 race 测试通过；mock 测试不需要真实 GitHub 凭据。

## 手动浏览器验证

1. `task run`
2. 打开管理页“供应商”，新增 GitHub Copilot 源并填写名称及参数，点击“使用 GitHub 授权”。
3. 弹窗应显示 user code 与 GitHub 地址，不应要求粘贴 token。
4. 在浏览器打开地址输入 user code 并批准。
5. 回到管理页等待状态变为已授权；源列表出现 Copilot 源，刷新后仍在。
6. 对已有 Copilot 源重复授权，源数量不变，旧源在新授权完成前可用。
7. 等待阶段点击取消，已有源配置不变，可重新发起。

## 安全抽查

- 查看 gateway.log，确认没有 `ghu_` / `gho_` / Authorization。
- 查看浏览器 Network，确认 start/status/cancel 无 access token。
- 确认 /v1 responses 和 /admin事件流无凭据字段。
