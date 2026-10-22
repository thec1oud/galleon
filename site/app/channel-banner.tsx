import { channel } from "@/lib/channel";

// ChannelBanner tells readers of the dev site and of branch previews that
// they aren't reading the stable docs. The stable site shows nothing.
export function ChannelBanner() {
  const c = channel();
  if (c.kind === "stable") return null;
  const what =
    c.kind === "dev"
      ? "These are the docs for the dev branch, which may describe work that hasn't reached master."
      : `This is a preview of the docs on branch ${c.branch}.`;
  return (
    <div role="note" className="border-b border-amber-300 bg-amber-100 px-4 py-2 text-center text-sm text-amber-950 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-100">
      {what}
      {c.stableURL && (
        <>
          {" "}
          <a href={c.stableURL} className="font-medium underline">Read the stable docs</a>
        </>
      )}
    </div>
  );
}
