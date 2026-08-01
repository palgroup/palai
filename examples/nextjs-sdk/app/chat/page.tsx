import { CodingChat } from "@/components/coding-chat";

// A Server Component shell, the shape app/page.tsx and app/compare/page.tsx both use: the interaction lives
// in the Client Component, which talks only to this app's own routes and never to the control plane.
export default function Page() {
  return <CodingChat />;
}
