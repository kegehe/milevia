// 冲突标记解析与应用工具（纯函数）。
//
// 输入是带 git 冲突标记的整份文本，形如：
//   <<<<<<< ours/HEAD
//   ... 当前侧内容
//   ||||||| base            （仅 diff3 风格存在）
//   ... 共同祖先内容
//   =======
//   ... 传入侧内容
//   >>>>>>> theirs/feature
// 工具负责把文本拆成冲突块、按选择（ours/theirs/both）替换某个块、统计未解决数。

export type ConflictChoice = "ours" | "theirs" | "both";

export type ConflictBlock = {
  ours: string;
  theirs: string;
  base: string | null;
  startLine: number;
  endLine: number;
};

function normalize(text: string): string {
  return text.replace(/\r\n/g, "\n");
}

// splitLines 按行拆分并保留结尾空串，以便 join("\n") 能还原末尾换行。
function splitLines(text: string): string[] {
  const normalized = normalize(text);
  if (normalized === "") return [];
  return normalized.split("\n");
}

function contentLines(content: string): string[] {
  if (content === "") return [];
  return content.split("\n");
}

// parseConflictBlocks 解析当前文本中所有未解决冲突块。损坏的标记会被跳过，
// 保证返回的每个块都能安全地整体替换。
export function parseConflictBlocks(text: string): ConflictBlock[] {
  const lines = splitLines(text);
  const blocks: ConflictBlock[] = [];
  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (!line.startsWith("<<<<<<< ")) {
      index++;
      continue;
    }
    const startLine = index;
    index++;
    const ours: string[] = [];
    const base: string[] = [];
    const theirs: string[] = [];
    let sawBaseHeader = false;
    while (index < lines.length && !lines[index].startsWith("||||||| ") && !lines[index].startsWith("=======")) {
      ours.push(lines[index]);
      index++;
    }
    if (index < lines.length && lines[index].startsWith("||||||| ")) {
      sawBaseHeader = true;
      index++;
      while (index < lines.length && !lines[index].startsWith("=======")) {
        base.push(lines[index]);
        index++;
      }
    }
    if (index < lines.length && lines[index].startsWith("=======")) {
      index++;
    }
    while (index < lines.length && !lines[index].startsWith(">>>>>>> ")) {
      theirs.push(lines[index]);
      index++;
    }
    if (index >= lines.length || !lines[index].startsWith(">>>>>>> ")) {
      break; // 标记损坏，不再继续解析
    }
    const endLine = index;
    index++;
    blocks.push({
      ours: ours.join("\n"),
      theirs: theirs.join("\n"),
      base: sawBaseHeader ? base.join("\n") : null,
      startLine,
      endLine,
    });
  }
  return blocks;
}

// resolveConflictBlock 把当前文本中的第 index 个未解决冲突块替换为所选内容。
export function resolveConflictBlock(text: string, index: number, choice: ConflictChoice): string {
  const blocks = parseConflictBlocks(text);
  const block = blocks[index];
  if (!block) return text;
  const lines = splitLines(text);
  const replacement =
    choice === "ours"
      ? contentLines(block.ours)
      : choice === "theirs"
        ? contentLines(block.theirs)
        : [...contentLines(block.ours), ...contentLines(block.theirs)];
  lines.splice(block.startLine, block.endLine - block.startLine + 1, ...replacement);
  return lines.join("\n");
}

// countConflictMarkers 统计当前文本中尚未解决的冲突块数量。
export function countConflictMarkers(text: string): number {
  let count = 0;
  for (const line of splitLines(text)) {
    if (line.startsWith("<<<<<<< ")) count++;
  }
  return count;
}
