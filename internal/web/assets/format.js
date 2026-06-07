// Pure scalar/string formatters for dates, sizes, token counts, SHAs and label
// maps. A dependency-free leaf. (formatTaints lives with the worker/queue
// rendering because it depends on the value() key reader.)

export function formatLabels(labels) {
  if (!labels || typeof labels !== "object") return "";
  return Object.entries(labels).map(([key, value]) => `${key}=${value}`).sort().join(", ");
}

export function formatDate(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString();
}

export function formatBytes(value) {
  const bytes = Number(value || 0);
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  let size = bytes;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  const precision = unit === 0 || size >= 10 ? 0 : 1;
  return `${size.toFixed(precision)} ${units[unit]}`;
}

export function formatTokenCount(value) {
  const tokens = Number(value || 0);
  if (!Number.isFinite(tokens) || tokens <= 0) return "0";
  if (tokens >= 1000000) return `${Number((tokens / 1000000).toFixed(tokens % 1000000 === 0 ? 0 : 1))}M`;
  if (tokens >= 1000) return `${Number((tokens / 1000).toFixed(tokens % 1000 === 0 ? 0 : 1))}K`;
  return String(tokens);
}

export function formatRelative(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

export function shortSHA(value) {
  const sha = String(value || "");
  return sha.length > 12 ? sha.slice(0, 12) : sha;
}
