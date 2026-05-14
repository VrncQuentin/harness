import { access, readFile } from "node:fs/promises";
import path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const MEMORY_DIR = ".pi-memory";

type Layer = {
  label: string;
  relativePath: string;
  required?: boolean;
};

const layers: Layer[] = [
  { label: "global rules", relativePath: "global/rules.md", required: true },
  { label: "user profile", relativePath: "global/user.md" },
  { label: "global facts", relativePath: "global/facts.md" },
  { label: "project rules", relativePath: "projects/default/rules.md" },
  { label: "agent notes", relativePath: "agents/coder/notes.md" },
];

async function readMemoryLayers(cwd: string): Promise<string[]> {
  const root = path.join(cwd, MEMORY_DIR);
  const contents: string[] = [];

  for (const layer of layers) {
    const filePath = path.join(root, layer.relativePath);
    try {
      const text = (await readFile(filePath, "utf8")).trim();
      if (text.length > 0) {
        contents.push(`## ${layer.label}\n${text}`);
      }
    } catch (error) {
      if (layer.required && (error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw error;
      }
    }
  }

  return contents;
}

async function memoryRootExists(cwd: string): Promise<boolean> {
  try {
    await access(path.join(cwd, MEMORY_DIR));
    return true;
  } catch {
    return false;
  }
}

export default function harnessMemory(pi: ExtensionAPI) {
  pi.on("before_agent_start", async (event, ctx) => {
    if (!(await memoryRootExists(ctx.cwd))) return;

    const memory = await readMemoryLayers(ctx.cwd);
    if (memory.length === 0) return;

    return {
      systemPrompt: `${event.systemPrompt}\n\n# Harness Memory\n${memory.join("\n\n")}`,
    };
  });

  pi.registerCommand("memory-status", {
    description: "Show Pi memory harness status",
    handler: async (_args, ctx) => {
      const root = path.join(ctx.cwd, MEMORY_DIR);
      const exists = await memoryRootExists(ctx.cwd);
      const loaded = exists ? (await readMemoryLayers(ctx.cwd)).length : 0;

      ctx.ui.notify(
        exists
          ? `Memory root: ${root}; loaded layers: ${loaded}`
          : `No ${MEMORY_DIR} directory found for this project.`,
        exists ? "info" : "warning",
      );
    },
  });
}
