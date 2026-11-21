// OrielHub 給 pi 的審批擴充（docs/acceptance/m3.md PI03、PI30、PI31）。
//
// pi 執行工具前不問任何人，所以審批經 OrielHub 時，轉接器把這個檔案寫到只有本使用者能讀寫的暫存路徑，
// 以 `-e <路徑>` 啟動 pi。不是唯讀的工具呼叫都等 OrielHub 的回答：轉接器收到的是標題為
// "orielhub.permission" 的擴充 UI 請求，內容是 JSON {toolCallId, toolName, input}。
//
// 只用 `import type`：pi 以 jiti 載入擴充，型別匯入不留下對 pi 套件的執行期依賴。
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// pi 內建的唯讀工具（內建工具不帶自己的 annotations）。只認真的內建工具：別的擴充可以註冊或覆寫同名的
// 工具，它的 sourceInfo.path 就不是 "builtin:<名字>"。
const readOnly = new Set(["read", "grep", "find", "ls"]);

export default function (pi: ExtensionAPI) {
	// 轉接器以 get_commands 找這個命令，確認擴充已載入。
	pi.registerCommand("orielhub-approvals", {
		description: "Tool calls ask OrielHub before they run",
		handler: async () => {},
	});

	pi.on("tool_call", async (event, ctx) => {
		const tool = pi.getAllTools().find((t) => t.name === event.toolName);
		const builtin = tool?.sourceInfo?.path === `builtin:${event.toolName}`;
		if (builtin && readOnly.has(event.toolName)) return undefined;
		if (tool?.annotations?.readOnlyHint === true) return undefined;
		// 一輪被中斷時 ctx.signal 結束這個詢問：結果是不允許。
		const allowed = await ctx.ui.confirm(
			"orielhub.permission",
			JSON.stringify({ toolCallId: event.toolCallId, toolName: event.toolName, input: event.input }),
			{ signal: ctx.signal },
		);
		if (!allowed) return { block: true, reason: "Denied in OrielHub" };
		return undefined;
	});
}
