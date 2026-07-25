
import { useState } from "react";
import { AlertTriangle, Database, Download, FileText, Trash2 } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

export function MemorySettings() {
  const { conversations, deleteConversation } = useSettingsStore();
  const [showClearConfirm, setShowClearConfirm] = useState(false);

  function exportConversations(format: "json" | "markdown") {
    let content: string;
    let filename: string;
    const ts = new Date().toISOString().split("T")[0];

    if (format === "json") {
      content = JSON.stringify(conversations, null, 2);
      filename = `claude-code-conversations-${ts}.json`;
    } else {
      content = conversations
        .map((conversation) => {
          const messages = conversation.messages
            .map((message) => {
              const role = message.role === "user" ? "**You**" : "**Claude**";
              const text =
                typeof message.content === "string"
                  ? message.content
                  : message.content
                      .filter((block) => block.type === "text")
                      .map((block) => (block as { type: "text"; text: string }).text)
                      .join("\n");
              return `${role}\n\n${text}`;
            })
            .join("\n\n---\n\n");
          return `# ${conversation.title}\n\n${messages}`;
        })
        .join("\n\n====\n\n");
      filename = `claude-code-conversations-${ts}.md`;
    }

    const blob = new Blob([content], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = filename;
    anchor.click();
    URL.revokeObjectURL(url);
  }

  function clearAllConversations() {
    const ids = conversations.map((conversation) => conversation.id);
    ids.forEach((id) => deleteConversation(id));
    setShowClearConfirm(false);
  }

  const totalMessages = conversations.reduce((sum, conversation) => {
    return sum + conversation.messages.length;
  }, 0);
  const pinnedCount = conversations.filter((conversation) => conversation.isPinned).length;
  const memoryTone = showClearConfirm ? "danger" : conversations.length > 0 ? "warn" : "good";
  const memoryTitle = showClearConfirm
    ? "Conversation deletion is armed"
    : conversations.length > 0
      ? "Local browser records are retained"
      : "No local conversations retained";
  const memoryDescription = showClearConfirm
    ? "Use the delete row below to confirm or cancel. Export first if the records should survive this browser."
    : conversations.length > 0
      ? "Export records before clearing them. Privacy telemetry controls stay separate from retained browser data."
      : "Nothing is stored in this browser conversation list yet.";

  return (
    <div>
      <SectionHeader
        eyebrow="Safety"
        title="Memory & Data"
        description="Control retained browser data separately from privacy and telemetry."
      />

      <SectionActionStrip
        icon={Database}
        title={memoryTitle}
        description={memoryDescription}
        stateLabel={showClearConfirm ? "armed" : conversations.length > 0 ? "retained" : "empty"}
        stateTone={memoryTone}
        metrics={[
          {
            label: "Conversations",
            value: `${conversations.length}`,
            tone: conversations.length > 0 ? "warn" : "good",
          },
          {
            label: "Messages",
            value: `${totalMessages}`,
            tone: totalMessages > 0 ? "neutral" : "good",
          },
          {
            label: "Pinned",
            value: `${pinnedCount}`,
            tone: pinnedCount > 0 ? "good" : "neutral",
          },
        ]}
        actions={[
          {
            label: "Export JSON",
            icon: Download,
            onClick: () => exportConversations("json"),
            disabled: conversations.length === 0,
            tone: "neutral",
          },
          {
            label: "Export Markdown",
            icon: FileText,
            onClick: () => exportConversations("markdown"),
            disabled: conversations.length === 0,
            tone: "neutral",
          },
          {
            label: "Arm clear history",
            icon: Trash2,
            onClick: () => setShowClearConfirm(true),
            disabled: conversations.length === 0 || showClearConfirm,
            tone: "danger",
          },
        ]}
      />

      <SettingRow
        label="Export conversations"
        description={`Export all ${conversations.length} conversations (${totalMessages} messages).`}
        scope="Persisted Data"
        risk="medium"
      >
        <div className="flex flex-wrap gap-2">
          <button
            onClick={() => exportConversations("json")}
            className={cn(
              "flex min-h-11 items-center gap-1.5 rounded-md border border-surface-700 px-3 py-2 text-xs",
              "text-surface-300 transition-colors hover:bg-surface-800 hover:text-surface-100 active:translate-y-px"
            )}
            type="button"
          >
            <Download className="h-3.5 w-3.5" />
            JSON
          </button>
          <button
            onClick={() => exportConversations("markdown")}
            className={cn(
              "flex min-h-11 items-center gap-1.5 rounded-md border border-surface-700 px-3 py-2 text-xs",
              "text-surface-300 transition-colors hover:bg-surface-800 hover:text-surface-100 active:translate-y-px"
            )}
            type="button"
          >
            <Download className="h-3.5 w-3.5" />
            Markdown
          </button>
        </div>
      </SettingRow>

      <SettingRow
        label="Clear conversation history"
        description="Permanently delete all conversations stored in this browser."
        scope="Persisted Data"
        risk="high"
      >
        {showClearConfirm ? (
          <div className="flex flex-wrap items-center justify-end gap-2">
            <span className="flex items-center gap-1 text-xs text-red-400">
              <AlertTriangle className="h-3.5 w-3.5" />
              Delete {conversations.length} conversations?
            </span>
            <button
              onClick={clearAllConversations}
              className="min-h-11 rounded-md bg-red-600 px-3 py-2 text-xs text-white transition-colors hover:bg-red-700 active:translate-y-px"
              type="button"
            >
              Delete all
            </button>
            <button
              onClick={() => setShowClearConfirm(false)}
              className="min-h-11 rounded-md px-3 py-2 text-xs text-surface-400 transition-colors hover:text-surface-200"
              type="button"
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            onClick={() => setShowClearConfirm(true)}
            disabled={conversations.length === 0}
            className={cn(
              "flex min-h-11 items-center gap-1.5 rounded-md border border-red-500/30 px-3 py-2 text-xs",
              "text-red-400 transition-colors hover:bg-red-500/10 active:translate-y-px",
              "disabled:cursor-not-allowed disabled:opacity-40"
            )}
            type="button"
          >
            <Trash2 className="h-3.5 w-3.5" />
            Clear all
          </button>
        )}
      </SettingRow>
    </div>
  );
}
