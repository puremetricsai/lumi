// Installed by Lumi. Pi has no built-in MCP client; this extension exposes Lumi's
// MCP tools with the names, descriptions, and schemas supplied by Lumi itself.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { spawn } from "node:child_process"

const binary = __LUMI_BINARY__
const args = __LUMI_ARGS__

function request(method: string, params?: unknown, signal?: AbortSignal): Promise<{ result: any; instructions?: string }> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(new Error("Lumi: aborted"))
    const child = spawn(binary, args, { stdio: ["pipe", "pipe", "pipe"] })
    let done = false
    let stderr = ""
    let buffer = ""
    let instructions: string | undefined
    const send = (message: unknown) => child.stdin.write(JSON.stringify(message) + "\n")
    const finish = (error?: Error, result?: any) => {
      if (done) return
      done = true
      clearTimeout(timer)
      signal?.removeEventListener("abort", abort)
      child.kill()
      if (error) reject(error)
      else resolve({ result, instructions })
    }
    const abort = () => finish(new Error("Lumi: aborted"))
    const timer = setTimeout(() => finish(new Error("Lumi: MCP request timed out")), method === "tools/list" ? 15_000 : 120_000)
    signal?.addEventListener("abort", abort, { once: true })
    child.stdin.on("error", (error: NodeJS.ErrnoException) => {
      if (error.code !== "EPIPE") finish(error)
    })
    child.stderr.setEncoding("utf8")
    child.stderr.on("data", (chunk: string) => { stderr = (stderr + chunk).slice(-4096) })
    child.stdout.setEncoding("utf8")
    child.stdout.on("data", (chunk: string) => {
      if (done) return
      buffer += chunk
      const lines = buffer.split("\n")
      buffer = lines.pop() ?? ""
      for (const line of lines) {
        if (!line.trim()) continue
        let message: any
        try { message = JSON.parse(line) } catch { return finish(new Error("Lumi: invalid MCP response")) }
        if (message.id === 1) {
          if (message.error) return finish(new Error(`Lumi: ${message.error.message}`))
          instructions = message.result?.instructions
          send({ jsonrpc: "2.0", method: "notifications/initialized" })
          send({ jsonrpc: "2.0", id: 2, method, params: params ?? {} })
        } else if (message.id === 2) {
          if (message.error) return finish(new Error(`Lumi: ${message.error.message}`))
          return finish(undefined, message.result)
        }
      }
    })
    child.on("error", (error: Error) => finish(new Error(`Lumi: ${error.message}`)))
    child.on("close", (code: number | null) => finish(new Error(stderr.trim() || `Lumi: exited with code ${code}`)))
    send({ jsonrpc: "2.0", id: 1, method: "initialize", params: {
      protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "pi", version: "1" },
    } })
  })
}

export default function (pi: ExtensionAPI) {
  pi.on("session_start", async () => {
    const { result, instructions } = await request("tools/list")
    // The server's cross-tool directions use MCP names; Pi exposes prefixed names.
    const piNames = (text: string) => result.tools.reduce(
      (value: string, tool: any) => value.replaceAll(tool.name, `lumi_${tool.name}`), text)
    for (const tool of result.tools) {
      pi.registerTool({
        name: `lumi_${tool.name}`,
        label: `Lumi: ${tool.name}`,
        description: piNames(tool.description),
        promptGuidelines: instructions ? [piNames(instructions)] : [],
        parameters: tool.inputSchema,
        async execute(_id, parameters, signal) {
          const { result } = await request("tools/call", { name: tool.name, arguments: parameters }, signal)
          const text = result.content.map((item: any) => item.text ?? JSON.stringify(item)).join("\n")
          if (result.isError) throw new Error(text)
          return { content: [{ type: "text", text }], details: undefined }
        },
      })
    }
  })
}
