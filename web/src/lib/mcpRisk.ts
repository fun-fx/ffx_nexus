export type ToolRisk = "read" | "write" | "network" | "data_export";

export function classifyToolRisk(name: string): ToolRisk {
  const n = name.toLowerCase().replace(/-/g, "_");
  if (/(email|send_mail|export|download|upload|share|forward)/.test(n)) return "data_export";
  if (/(http|fetch|request|webhook|browse|url|web_)/.test(n)) return "network";
  if (/(write|create|update|delete|put|patch|remove|insert|drop)/.test(n)) return "write";
  return "read";
}

export function riskTone(risk: string): "ok" | "warn" | "err" | "accent" | "neutral" {
  if (risk === "data_export") return "err";
  if (risk === "network" || risk === "write") return "warn";
  if (risk === "read") return "ok";
  return "neutral";
}
