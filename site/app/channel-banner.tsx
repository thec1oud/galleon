import { channel } from "@/lib/channel";

// ChannelBanner tells readers of the dev site and of branch previews that
// they aren't reading the stable docs. The stable site shows nothing. Like
// Fumadocs' own banner, it sticks to the top at a fixed height and sets
// --fd-banner-height, which the docs layout leaves room for.
export function ChannelBanner() {
  const c = channel();
  if (c.kind === "stable") return null;
  const [long, short] =
    c.kind === "dev"
      ? ["These are the docs for the dev branch, which may describe work that hasn't reached master.", "Dev branch docs."]
      : c.branch
        ? [`This is a preview of the docs on branch ${c.branch}.`, "Preview docs."]
        : ["This is a preview deployment of the docs.", "Preview docs."];
  return (
    <div role="note" className="sticky top-0 z-40 flex h-12 items-center justify-center gap-1 overflow-hidden border-b border-amber-300 bg-amber-100 px-4 text-center text-sm text-amber-950 [.dark_&]:border-amber-800 [.dark_&]:bg-amber-950 [.dark_&]:text-amber-100">
      <style>{":root { --fd-banner-height: 3rem; }"}</style>
      <span className="truncate max-md:hidden">{long}</span>
      <span className="md:hidden">{short}</span>
      {c.stableURL && (
        <a href={c.stableURL} className="shrink-0 font-medium underline">Read the stable docs</a>
      )}
    </div>
  );
}
