# simpterm

极简终端复用器，类似阉割版 tmux。提供可后台挂起的持久终端会话。

## 安装

```bash
pixi run compile
```

编译后将 `simpterm` 加入 PATH 即可。

## 使用

```bash
simpterm n [name]                    # 新建会话
simpterm a <name|id>                 # 连接会话（Ctrl+\ 断开）
simpterm e <name|id> <timeout> <cmd> # 执行命令并获取输出
simpterm l                           # 列出会话
simpterm k <name|id>                 # 关闭会话
```

### 示例

```bash
simpterm n work              # 创建名为 work 的会话
simpterm a work              # 连接，按 Ctrl+\ 断开
simpterm e work 30 "pwd"     # 在 work 中执行 pwd
simpterm k work              # 关闭 work
```

`e` 命令适合 AI/脚本调用——会话状态（工作目录、环境变量等）在调用间保持。

## 许可证

Apache License 2.0
