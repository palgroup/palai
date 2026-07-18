import { LiveResponse } from "@/components/live-response";

// A Server Component shell. All streaming logic lives in the LiveResponse Client Component,
// which talks only to the /api/palai Route Handler — never to the control-plane, never with
// a key.
export default function Page() {
  return <LiveResponse />;
}
