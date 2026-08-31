import assert from "node:assert/strict";
import test from "node:test";

await import("../web/json-validator.js");

const { validate } = globalThis.EtcdStudioJSONValidator;

test("accepts valid JSON objects, arrays, and scalar values", () => {
  for (const value of ['{"enabled":true}', "[1,2,3]", '"text"', "42", "null"]) {
    assert.deepEqual(validate(value), { valid: true, message: "JSON 格式正确" });
  }
});

test("rejects an empty Value", () => {
  assert.deepEqual(validate("  \n"), { valid: false, message: "Value 为空，无法校验 JSON" });
});

test("reports invalid JSON with its line and column when available", () => {
  const result = validate('{\n  "enabled": true,\n  "version":\n}');
  assert.equal(result.valid, false);
  assert.match(result.message, /^JSON 格式错误：/);
  assert.match(result.message, /第 \d+ 行，第 \d+ 列/);
});
