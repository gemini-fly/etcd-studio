"use strict";

function jsonErrorLocation(text, message) {
  const lineAndColumn = /\bline\s+(\d+)\s+column\s+(\d+)\b/i.exec(message);
  if (lineAndColumn) {
    return { line: Number(lineAndColumn[1]), column: Number(lineAndColumn[2]) };
  }

  const positionMatch = /\bposition\s+(\d+)\b/i.exec(message);
  const position = positionMatch ? Number(positionMatch[1]) : text.length;
  const before = text.slice(0, Math.max(0, Math.min(position, text.length)));
  const lines = before.split("\n");
  return { line: lines.length, column: [...lines.at(-1)].length + 1 };
}

function validateJSON(text) {
  text = String(text);
  if (text.trim() === "") {
    return { valid: false, message: "Value 为空，无法校验 JSON" };
  }
  try {
    JSON.parse(text);
    return { valid: true, message: "JSON 格式正确" };
  } catch (error) {
    const location = jsonErrorLocation(text, error instanceof Error ? error.message : "");
    return {
      valid: false,
      message: `JSON 格式错误：第 ${location.line} 行，第 ${location.column} 列附近`,
    };
  }
}

globalThis.EtcdStudioJSONValidator = Object.freeze({ validate: validateJSON });
