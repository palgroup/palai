import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn is shadcn/ui's class merger, required by every AI Elements component. It is here rather than
// vendored per-component because the registry's generated imports all point at `@/lib/utils`.
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
