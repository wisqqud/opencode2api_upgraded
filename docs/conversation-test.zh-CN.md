# 同项目同会话连续对话测试

时间：2026-11-16 20:17（Asia/Shanghai）。源码提交：67ed3c0690f40503f3094c23e99c3dc8b8ea36de，测试前重新构建。

## 结果

固定project和session，连续四轮Chat Completions全部HTTP 200，回答均与预期完全一致。后续请求携带完整messages历史，包含实际assistant回复及第三轮返回的reasoning_content。此结果验证客户端携带历史的多轮对话，不表示网关或上游仅凭session自动保存并补回历史。

| 轮次 | 测试任务 | 实际回答 | HTTP | 耗时 |
| --- | --- | --- | --- | --- |
| 1 | 记住随机代号TEST-6774B35D及初始数量60 | 已记住 | 200 | 3.066秒 |
| 2 | 回忆代号与初始数量 | TEST-6774B35D 60 | 200 | 2.180秒 |
| 3 | 在初始数量上增加7 | 67 | 200 | 3.078秒 |
| 4 | 汇总代号、初始和增加后数量 | TEST-6774B35D 60 67 | 200 | 2.426秒 |

## 条件

- 模型deepseek-v4.1-flash-free，POST /v1/chat/completions，stream=false，max_completion_tokens=1024。
- 每轮固定project=c62049256e56cea1420a4e4c39a923e522a73ffb。
- 每轮固定session=ses_e1bb56776ffepj64ztbggCwDRq，同时传入x-opencode-session及x-opencode-session-id。
- 每轮携带一个客户端edit工具，使用Harness原描述以及file_path/old_string/new_string/replace_all参数，实际出站由当前代码归一为官方描述并使用clnt_别名；官方12工具前置。
- 后续请求messages数量分别为3、5、7，不仅重复发送独立请求。
- 临时直连实例监听127.0.0.1:18081；WARP关闭，proxies=[direct]，清除代理环境变量；匿名通道，无Zen/Go Key。
- 日志确认四轮同一session，attempts=1，@direct，无429或重试轮换。
- 未执行工具调用，因此本轮不覆盖多轮工具执行结果回传。

## 证据与清理

临时证据目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-conversation-test-dc87yeia/。

metadata.json保存身份及随机测试值；results.json保存每轮结果；turn-*-request.json保存不含鉴权的请求；conversation.py为复现脚本。临时配置含本地Key，未作为交付文件发布。

四轮request_id：req_a0c48e3e2a71a590099124e88c38fb53、req_1f074739c8df7e164952eb2029ff0bc5、req_7f291ab5486faeac56500c9093893a38、req_850eab1e011b4e28f6fb58db51a256bd。

测试结束停止临时实例。原配置及生产代码保持不变，未新增提交或推送。
