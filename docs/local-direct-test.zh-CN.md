# 本机直连调用测试

测试时间：2026-11-16 18:28—18:30（Asia/Shanghai）。

## 结论

当前本地源码构建的服务可以直接完成推理。最小请求返回HTTP 200和正文OK；额外加入特定客户端工具描述后返回429，替换为Test tool又返回200，再恢复原描述返回429，最后无工具复测200。此结果在本机直连项目实例上复现，不经过CPA。

## 实例与测试条件

- 源码HEAD：6dfdd1d25c0a43cbe52164cd5ed764a3d71bbbdd；本轮新构建，不使用项目目录中既有二进制。
- 本机原先没有项目服务监听。
- 临时实例地址：http://127.0.0.1:8080；仅回环监听，管理UI关闭。
- 原配置启用WARP；为测试直连，从现有配置制作临时副本，WARP关闭、proxies=[direct]、proxyfile为空，主动模型探测关闭。
- 测试进程清除HTTP_PROXY、HTTPS_PROXY、ALL_PROXY及小写同名环境变量；客户端使用无代理HTTP opener。
- 原server key仅在本机内存与临时权限受限配置中使用，没有输出其值。
- 无Zen/Go上游Key，使用匿名通道。请求日志均为attempts=1、@direct，无出口轮换。
- 模型：deepseek-v4.1-flash-free；接口：POST /v1/chat/completions。
- 所有推理请求相同用户消息：Reply with the single word OK. Do not call any tools.；stream=false，max_completion_tokens=1024。
- “无工具”指客户端没有声明工具，网关仍按原策略追加官方工具。
- 不同请求中project ID按当前代码随机生成，未锁定为同一值；观测到上游合成session相同。这是请求内容关联结果，不是对上游内部判定规则的完整证明。

## 结果

| 顺序 | 请求 | HTTP | 耗时 | 响应 |
| --- | --- | --- | --- | --- |
| 1 | GET /healthz | 200 | 0.004秒 | ready=true，status=ok |
| 2 | GET /v1/models，使用本地Key | 200 | 0.001秒 | 13个可用模型 |
| 3 | 无客户端工具 | 200 | 2.325秒 | OK |
| 4 | edit原描述 | 429 | 0.305秒 | Free usage limit reached；Retry-After=48653 |
| 5 | edit描述为Test tool | 200 | 2.813秒 | OK |
| 6 | edit原描述，再测 | 429 | 0.311秒 | Free usage limit reached；Retry-After=48608 |
| 7 | 无客户端工具，再测 | 200 | 3.235秒 | OK |

工具名称始终edit，schema始终为{"type":"object","properties":{}}。原描述为：

```text
Edit an existing UTF-8 text file by replacing literal text.
```

请求3、4、5、6、7对应request_id分别为：

```text
req_d3c8b59a67b85a6821d6b5bc8063a5dc
req_078f187f4e9633ee57ec6fc7311c562e
req_b8b1f40542d8296a9d716b54c278ba9e
req_e74aaef8e41d989d44c7dfc7c9ccd16b
req_50fceaf08f583a303f0fc442cd6b5345
```

## 证据与清理

临时证据目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-local-direct-test-pakzxw3p/。

其中results.json、control-results.json保存脱敏结果；test_direct.py与control.py保存复现脚本；各响应另行保存。config.json包含已有本地server key，仅用于本机测试，未作为交付文件发布。

原项目config.json保持WARP启用、原监听和管理UI设置，未修改。测试结束停止临时实例，生产代码无改动，未提交或推送Git。

## 补充：官方 edit 描述对照（18:38）

按用户要求再次启动同一临时直连实例，测试客户端edit工具使用官方2.3.0描述的结果。官方描述取自internal/gateway/opencode_tools.json，并确认与此前独立官方客户端捕获逐字相同，共950字符。

本轮所有请求固定同一个x-opencode-project，日志确认合成session相同；模型、用户消息、工具名称、输出预算和direct出口相同，均单次上游尝试。前四项工具schema固定为空对象，第五项额外换成官方schema。没有执行工具调用。

| 顺序 | 描述/参数 | HTTP | 耗时 | 正文 |
| --- | --- | --- | --- | --- |
| 1 | 官方描述，空对象schema | 200 | 3.301秒 | OK |
| 2 | Harness原描述，空对象schema | 429 | 1.022秒 | Free usage limit reached；Retry-After=48105 |
| 3 | Test tool，空对象schema | 200 | 2.038秒 | OK |
| 4 | 官方描述，空对象schema，复测 | 200 | 2.940秒 | OK |
| 5 | 官方描述及官方schema | 200 | 3.094秒 | OK |

本轮证明：在保留现有网关整形逻辑的情况下，仅将该客户端工具的description换成官方原文即可通过此次推理测试；不需要同时更换schema。它不证明批量替换全部工具描述都能通过，也未验证返回工具调用后的实际参数兼容性。网关仍把该客户端edit改成clint_别名，并追加官方12工具，因此不是整份请求完全照搬官方。

结果目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-local-direct-test-pakzxw3p/official-edit-20261116-183811/，包含results.json及每项请求（不含鉴权）原文。复现脚本位于上级official_edit_test.py。

完成后停止临时实例，原配置及生产代码保持不变。

## 补充：修复前后同条件对照（19:23）

18:38官方描述请求确实返回200；19:12修复后回归全部429。但修复后的Test tool及Harness原描述也被统一改写成官方描述，因此它们不再是独立的描述对照，不能以“全429”断定整体额度耗尽。两轮服务重启还改变了合成session，脚本每轮随机生成project；历史成功与后续失败不是只改代码的单变量实验。

19:23保留旧二进制与修复二进制，使用相同配置、同一显式session、同一project和direct出口，先本地捕获出站body，再访问真实上游：

| 版本 | 请求 | 实际上游状态 | 耗时 |
| --- | --- | --- | --- |
| 修复前 | 无客户端工具 | 200，OK | 3.635秒 |
| 修复前 | 客户端edit使用官方描述 | 429 | 0.312秒 |
| 修复后 | 无客户端工具 | 200，OK | 4.271秒 |
| 修复后 | 客户端edit使用官方描述 | 429 | 0.329秒 |

独立本地HTTP fixture仅用于观察出站请求，其模拟200不代表真实上游成功。捕获确认以下三项最终body逐字节相同：旧版输入官方描述、新版输入官方描述、新版输入Harness原描述。共同SHA-256为737782a5593433fbea5ec80225f1b1dcbe29fe1949fa9cdc09c6febbcf3e1ee2。trace随机头不包含在body哈希中。

这说明修复没有改变“客户端已提供官方描述”时的请求body，也确实把Harness原描述归一成了同样body；当前旧版也复现429，因此现有证据不支持将变化归因于本次描述修复。与此同时最小请求持续200，也不支持直接断定全局额度耗尽。上游内部内容判定、状态/额度条件或实际公网出口是否发生变化，仍未确定；未抓到18:38历史公网出口，direct标签只代表无显式代理，并不证明不同时刻公网IP相同。

证据目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-local-direct-test-pakzxw3p/compare-versions-20261116-192320/。results.json包含明确标为capture或live的结果；*-outbound.json为最终出站载荷；*-*.log为临时服务日志。compare_versions.py位于上级目录。临时服务均已退出，8080和18080均无监听。

## 补充：同时更换 project 与 session（19:28）

按用户要求，使用修复后的二进制、direct出口，生成两组全新的project及session。project为40位随机hex；session使用当前毫秒时间的descending布局与14位随机Base62后缀。显式同时设置x-opencode-session与x-opencode-session-id，服务日志确认两组session逐字保留。每组内project/session不变，组间全部更换；单次上游尝试，没有重试轮换。

| 新身份组 | 请求顺序 | 无客户端工具 | 官方描述edit |
| --- | --- | --- | --- |
| 1 | edit后基线 | 200，OK，3.214秒 | 429，1.849秒 |
| 2 | 基线后edit | 200，OK，9.022秒 | 429，0.327秒 |

第1组project=23b14117b6d6232be60602d68d5cc104f58cea90，session=ses_e1be2c729ffec7Z2aSoiBStzJI。第2组project=d990fe686905e6b8286cb0174259bdfa041aba5f，session=ses_e1be2c729ffebFIidQ3vqo5Jqq。

两次429的Retry-After分别为45117和45103秒。结果不支持“仅旧project/session状态导致当前edit请求失败”的解释，也未恢复18:38的成功结果。请求形状差异仍存在：基线由网关补12个官方工具，而客户端edit经过哈希改名后作为额外第13个工具保留。此次未进一步测试工具数量或重复定义是否构成条件。

证据目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-local-direct-test-pakzxw3p/fresh-identity-20261116-192802/，含identities.json、results.json与service.log。脚本fresh_identity_test.py位于上级目录。测试服务已退出，8080无监听。

## 补充：官方工具前置、精确去重及 clnt_ 别名（19:46）

按用户指定策略修改：完整官方工具集固定前置，客户端原始完整工具定义与官方完全一致则复用对应项；其它客户端工具使用clnt_加SHA-256前24十六进制字符，放在官方集后面。保留先前edit描述归一化；精确去重判断发生在描述归一化前，strict/schema等差异仍保留别名工具。

完整go test ./...、git diff --check及构建通过。回归覆盖Chat/Responses/Anthropic、V1/V2、去重、官方顺序、字段差异、identity名称映射、历史与tool_choice、流式及非流式名称还原、legacy functions。

重新构建opencode2api-prefix后，使用同一全新project及session、同一模型与direct出口测试。先以本地HTTP fixture捕获最终body，再使用相同二进制和配置（恢复真实upstream）执行推理。capture阶段模拟200不作为上游结果。

| 请求 | 最终工具列表 | 实际上游HTTP | 耗时 | 正文 |
| --- | --- | --- | --- | --- |
| 无客户端工具 | 12个官方工具 | 200 | 10.580秒 | OK |
| Harness原edit描述及空对象schema | 官方12工具 + clnt_262121c5372be8af3ae6ff0d；edit描述已归一为官方原文 | 200 | 5.826秒 | OK |
| 与官方edit完整一致，含strict:false | 只保留官方12工具，无额外别名项 | 200 | 5.869秒 | OK |

日志确认三项均attempts=1且@direct。无客户端工具与精确复用edit的最终body逐字节相同，SHA-256=e358635e71dc5b323ce4c9b4ccc7248b5ceb13c623b925d573bd48c88bd42835。别名工具位于最后，符合新策略。仅此轮测试通过，尚未验证全部实际客户端工具集，工具顺序和别名同时变更也没有进一步消融来归因哪项改善了结果。

证据目录：/var/folders/23/v296v21n13x_grsqs0y588ch0000gn/T/opencode-local-direct-test-pakzxw3p/prefix-strategy-20261116-194632/。results.json区分capture/live，*-outbound.json保存最终payload，live-service.log保存实际路由。复现脚本prefix_strategy_test.py在上级目录。

临时服务已退出，8080和18080均无监听。原配置未改，没有提交或推送。

## 补充：是否仍需 edit 描述归一化（19:51）

用户询问新策略下是否可以移除edit描述替换。使用Go overlay仅在临时构建中移除该分支，项目源文件保持原样。保留官方工具前置、精确去重及clnt_别名。先本地捕获确认别名edit保留Harness原59字符描述，再做真实直连：

| 处理 | 请求 | HTTP | 耗时 |
| --- | --- | --- | --- |
| 关闭描述替换 | 无客户端工具 | 200，OK | 2.580秒 |
| 关闭描述替换 | Harness原edit描述、空schema | 429 | 0.310秒 |
| 关闭描述替换 | 完全一致官方edit复用 | 200，OK | 3.262秒 |
| 恢复描述替换 | 同一Harness原edit请求 | 200，OK | 2.528秒 |

最后一项与前面使用同一个显式project=716160ed7b202bd2ccbbe0e02b63b56984eafc06、session=ses_e1bcda69affeeZr8h0Am9b7OW7、同一direct配置，单次上游尝试。结果支持当前继续保留客户端edit描述归一化；官方前置及别名变更并没有单独消除该描述的429。没有进一步证明上游内部规则或其它工具的行为。

证据目录位于同一临时根目录下：no-edit-normalize-20261116-195107/（含最终body捕获）、restore-edit-normalize-20261116-195136/（同身份恢复测试）。overlay和脚本分别为no-edit-normalize-overlay.json、no_edit_normalize_test.py、restore_edit_normalize_test.py。临时实例均已结束。
