export type AnsiSegment = {
	text: string;
	className?: string;
};

type AnsiStyle = {
	color?: string;
	bold: boolean;
	dim: boolean;
};

const sgrPattern = /\x1b\[([0-?]*)([ -/]*)m/g;
const oscPattern = /\x1b\][\s\S]*?(?:\x07|\x1b\\)/g;
const controlPattern = /\x1b\[[0-?]*[ -/]*[@-~]/g;

const colorClasses: Record<number, string> = {
	30: "ansi-black", 31: "ansi-red", 32: "ansi-green", 33: "ansi-yellow",
	34: "ansi-blue", 35: "ansi-magenta", 36: "ansi-cyan", 37: "ansi-white",
	90: "ansi-bright-black", 91: "ansi-bright-red", 92: "ansi-bright-green", 93: "ansi-bright-yellow",
	94: "ansi-bright-blue", 95: "ansi-bright-magenta", 96: "ansi-bright-cyan", 97: "ansi-bright-white",
};

function classNameFor(style: AnsiStyle): string | undefined {
	const classes = [style.color, style.bold ? "ansi-bold" : "", style.dim ? "ansi-dim" : ""].filter(Boolean);
	return classes.length ? classes.join(" ") : undefined;
}

function applySgr(style: AnsiStyle, parameters: string): AnsiStyle {
	const next = { ...style };
	const codes = parameters === "" ? [0] : parameters.split(";").map((code) => Number.parseInt(code || "0", 10));
	for (const code of codes) {
		if (code === 0) { next.color = undefined; next.bold = false; next.dim = false; }
		else if (code === 1) next.bold = true;
		else if (code === 2) next.dim = true;
		else if (code === 22) { next.bold = false; next.dim = false; }
		else if (code === 39) next.color = undefined;
		else if (colorClasses[code]) next.color = colorClasses[code];
	}
	return next;
}

// Convert terminal SGR controls into safe React render segments. Other terminal
// controls are removed because browsers cannot interpret cursor movement.
export function toAnsiSegments(value: string): AnsiSegment[] {
	const text = value.replace(oscPattern, "");
	const segments: AnsiSegment[] = [];
	let style: AnsiStyle = { bold: false, dim: false };
	let cursor = 0;
	const appendText = (content: string) => {
		const clean = content.replace(controlPattern, "");
		if (clean) segments.push({ text: clean, className: classNameFor(style) });
	};

	for (const match of text.matchAll(sgrPattern)) {
		const index = match.index ?? 0;
		if (index > cursor) appendText(text.slice(cursor, index));
		style = applySgr(style, match[1]);
		cursor = index + match[0].length;
	}
	if (cursor < text.length || segments.length === 0) appendText(text.slice(cursor));
	return segments;
}
