# Cody

## 核心特性

### ReAct Agent 与分层架构

- 基于 ReAct（工具调用循环）驱动 LLM 自主完成读代码、写文件、执行命令、多轮验证等编程任务
- 分层架构支持多模型接入（Anthropic / OpenAI / OpenAI-compatible）、MCP 工具扩展与多 Agent 协作（子代理、Team 调度）

### 上下文与 Token 优化

- MCP 工具延迟加载：默认只注入核心工具，模型通过 ToolSearch 按需发现并加载 MCP 工具；百级工具场景下 tools 参数 Token 开销下降约 85%
- 两级渐进上下文压缩：工具结果预算（Layer 1）+ 自动摘要压缩（Layer 2），长会话持续运行不溢出；压缩边界写入会话日志，后续可重建恢复

### 安全与权限

- 多层权限校验：危险命令黑名单、路径沙箱、规则引擎、会话级授权与权限模式逐层过滤，高危操作弹窗确认
- Bash 命令可在 OS 沙箱内执行（Linux bubblewrap），强制隔离读写路径与网络权限
- 会话记忆系统：自动提取项目级与用户级关键经验，巩固后跨会话复用

### 可逆性

- 基于 git worktree 实现 Agent 修改隔离与过期自动清理
- 文件历史快照配合 `/rewind` 提供代码与对话回滚能力，复杂任务全程可逆

## 启动方法

创建`.cody/config.yaml`文件

```yaml
providers:
  - name: anthropic-official
    protocol: anthropic   # 支持 anthropic / openai / openai-compat 三种协议
    base_url: https://api.deepseek.com/anthropic
    api_key: "sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
    model: deepseek-v4-flash
    thinking: true        # 是否开启 extended thinking 让模型在正式回答前先输出一段内部推理，再给出答案

mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]

enable_coordinator_mode: false   # leader是否只调度
```

执行编译

```bash
go build -o cody ./cmd/cody/
```

## TUI 操作指南

### 启动与模型选择

启动后若配置了多个 provider，会先进入模型选择界面：

| 按键      | 功能                             |
| --------- | -------------------------------- |
| `↑` / `k` | 上移选择                         |
| `↓` / `j` | 下移选择                         |
| `Enter`   | 使用当前选中的 provider 进入对话 |

只配置一个 provider 时会直接进入对话界面。

### 对话快捷键

| 按键                             | 功能                                                         |
| -------------------------------- | ------------------------------------------------------------ |
| `Enter`                          | 发送消息或执行斜杠命令                                       |
| `Ctrl+J`                         | 插入换行（多行输入）                                         |
| `Ctrl+C`                         | 生成中：取消当前生成并保留已生成内容；空闲时：退出程序       |
| `Ctrl+O`                         | 展开/收起所有工具调用、子代理等折叠块                        |
| `Shift+Tab`                      | 循环切换权限模式（default → acceptEdits → plan → bypassPermissions） |
| `↑` / `↓`                        | 在输入框首行/末行时浏览历史输入                              |
| `PgUp` / `PgDn` / `Home` / `End` | 滚动对话视图                                                 |
| `Esc`                            | 生成中：将当前任务转入后台，完成后会收到通知                 |

输入 `/` 会弹出斜杠命令补全菜单，输入 `@` 会弹出文件引用补全菜单：

| 按键      | 功能                                           |
| --------- | ---------------------------------------------- |
| `↑` / `↓` | 选择匹配项                                     |
| `Enter`   | `/` 菜单直接执行命令；`@` 菜单插入选中文件路径 |
| `Tab`     | 将当前匹配项补全到输入框                       |
| `Esc`     | 关闭菜单                                       |

### 斜杠命令

| 命令          | 别名     | 说明                                                         |
| ------------- | -------- | ------------------------------------------------------------ |
| `/help`       | `h`, `?` | 显示可用命令；`/help <command>` 查看命令详情                 |
| `/mcp`        |          | 显示 MCP 服务器连接状态                                      |
| `/clear`      |          | 清空当前对话并开启全新会话                                   |
| `/compact`    | `c`      | 压缩对话上下文                                               |
| `/status`     | `s`      | 显示权限模式、token 用量、工具数量、记忆、模型和工作目录     |
| `/memory`     |          | 管理自动记忆：`/memory list` 列出，`/memory clear` 清空      |
| `/plan`       | `p`      | 进入只读 Plan 模式；带参数时可直接发送计划要求               |
| `/session`    |          | 查看当前会话信息：`/session info` 或 `/session list`         |
| `/permission` | `perm`   | 权限管理：`/permission mode default\|acceptEdits\|plan\|bypassPermissions` |
| `/resume`     | `r`      | 恢复历史会话；无参数时打开会话选择列表，也可指定会话 ID 或序号 |
| `/rewind`     |          | 打开检查点回滚界面，可恢复代码和/或对话                      |
| `/skills`     |          | 列出可用 skills；`/skills reload` 热重载磁盘上的 skills      |
| `/sandbox`    |          | 打开 OS 级沙箱模式选择                                       |
| `/review`     |          | 审查当前 git diff，可附带额外关注点                          |

### 权限模式

| 模式                | 行为                                 |
| ------------------- | ------------------------------------ |
| `default`           | 读取自动放行，写入和命令需要确认     |
| `acceptEdits`       | 自动接受文件编辑，命令仍需要确认     |
| `plan`              | 只读 Plan 模式，仅允许写入 plan 文件 |
| `bypassPermissions` | 所有操作自动放行                     |

可通过 `Shift+Tab` 循环切换，或用 `/permission mode <mode>` 直接设置。

### 对话框快捷键

| 场景           | 操作                                                         |
| -------------- | ------------------------------------------------------------ |
| 权限确认弹窗   | `↑`/`↓` 选择，`Enter` 确认，`Esc` 拒绝                       |
| Plan 审批弹窗  | `↑`/`↓` 或 `k`/`j` 选择，`Enter` 确认；选择反馈输入后 `Shift+Tab` 带反馈批准；`Esc` 关闭 |
| 沙箱模式弹窗   | `↑`/`↓` 或 `k`/`j` 选择，`Enter` 确认，`Esc` 关闭            |
| Agent 提问弹窗 | `↑`/`↓` 或 `k`/`j` 选择，`Space` 切换多选，`Tab`/`→` 下一题，`Shift+Tab`/`←` 上一题，`Enter` 确认并提交，`Esc` 取消 |
| 回滚检查点选择 | `↑`/`↓` 选择检查点，`Enter` 进入恢复选项，`Esc` 返回         |
| 恢复选项选择   | `↑`/`↓` 选择恢复方式（代码+对话 / 仅对话 / 仅代码 / 放弃），`Enter` 执行，`Esc` 返回检查点列表 |
| 会话恢复列表   | `↑`/`↓` 选择，`Enter` 恢复，直接输入文字过滤，`Backspace` 删除，`Esc` 返回对话 |

###  Skills


Skills 放在项目 `.cody/skills/<skill-name>/SKILL.md`，通过 `/skills` 查看，用 `/<skill-name>` 直接调用，修改后用 `/skills reload` 热重载。