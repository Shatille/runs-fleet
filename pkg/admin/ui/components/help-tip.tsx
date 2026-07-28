export function HelpTip({ text }: { text: string }) {
  return (
    <span
      title={text}
      aria-label={text}
      role="note"
      tabIndex={0}
      className="ml-1 inline-flex h-3.5 w-3.5 shrink-0 cursor-help select-none items-center justify-center rounded-full border border-gray-400 align-middle text-[9px] font-bold normal-case leading-none text-gray-500 dark:border-gray-500 dark:text-gray-400"
    >
      ?
    </span>
  );
}
