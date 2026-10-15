// User-facing conversation actions (shared by the sidebar items and the chat
// header menu): each reports success or failure with a toast.
import { unsupported } from "./api.js";
import { convs } from "./conversations.js";
import { toast, errorToast, confirmDialog } from "./ui.js";

function fail(e, what) {
  if (unsupported(e)) toast(what + " is not supported by this daemon", { error: true });
  else errorToast(e, what + " failed");
}

// displayTitle falls back to the last message preview for untitled (older)
// conversations.
export const displayTitle = (c) => (c && (c.title || c.last_message_preview)) || "New chat";

export async function rename(id, title) {
  const c = convs.get(id);
  title = title.trim();
  if (!c || !title || title === c.title) return;
  try { await convs.patch(id, { title }); } catch (e) { fail(e, "Rename"); }
}

export async function togglePin(id) {
  const c = convs.get(id);
  if (!c) return;
  try {
    await convs.patch(id, { pinned: !c.pinned });
    toast(c.pinned ? "Pinned" : "Unpinned");
  } catch (e) { fail(e, c.pinned ? "Unpin" : "Pin"); }
}

export async function toggleArchive(id) {
  const c = convs.get(id);
  if (!c) return;
  const archiving = !c.archived;
  try {
    await convs.patch(id, { archived: archiving });
    toast(archiving ? "Archived" : "Restored", archiving ? { action: { label: "Undo", run: () => toggleArchive(id) } } : {});
  } catch (e) { fail(e, archiving ? "Archive" : "Unarchive"); }
}

// remove asks for confirmation, then deletes; onDeleted runs on success.
export async function remove(id, onDeleted) {
  const c = convs.get(id);
  const ok = await confirmDialog({
    title: "Delete conversation?",
    body: "“" + displayTitle(c) + "” and all its messages will be deleted permanently. Pending approvals in it are canceled.",
    confirm: "Delete",
    danger: true,
  });
  if (!ok) return;
  try {
    await convs.remove(id);
    toast("Deleted");
    onDeleted?.();
  } catch (e) { fail(e, "Delete"); }
}

export async function exportMarkdown(id) {
  try { await convs.exportMarkdown(id); } catch (e) { fail(e, "Export"); }
}

// menuItems is the conversation menu (sidebar kebab and header).
export function menuItems(id, { onRename, onDeleted } = {}) {
  const c = convs.get(id) || {};
  return [
    { label: "Rename", icon: "pencil", run: () => onRename?.() },
    { label: c.pinned ? "Unpin" : "Pin", icon: c.pinned ? "unpin" : "pin", run: () => togglePin(id) },
    { label: c.archived ? "Unarchive" : "Archive", icon: c.archived ? "unarchive" : "archive", run: () => toggleArchive(id) },
    { label: "Export as Markdown", icon: "download", run: () => exportMarkdown(id) },
    "-",
    { label: "Delete", icon: "trash", danger: true, run: () => remove(id, onDeleted) },
  ];
}
