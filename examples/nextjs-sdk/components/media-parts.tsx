"use client";

// THE AGENT SHOWING YOU SOMETHING. This is the render half of palai.workspace.show_media: the tool put
// a screenshot or a recording in the artifact store and answered with an id, and the browser fetches
// the object through the relay that already existed.
//
// THE BYTES NEVER TOUCH THE CHAT PAYLOAD. `/api/palai/artifacts?id=…` streams the object with the
// server-side credential, so what travels through the event stream is an id and a caption. That is what
// makes a 20 MB screen recording renderable at all — a chat that carried the bytes would have to buffer
// them into a JSON frame.
//
// WHY IT IS AN <img>/<video> AND NOT A DOWNLOAD LINK: the whole point of the tool is the agent saying
// "look, this is how it turned out — is that right?", and a link is a question the user has to click to
// be asked.

export interface MediaPartData {
  artifactId: string | null;
  mediaType: string | null;
  caption?: string | null;
  path?: string | null;
  bytes?: number | null;
}

export function MediaPart({ data }: { data: MediaPartData }) {
  // AN ANSWER WITH NO ID IS NOT A PICTURE. The tool refuses rather than answering id-less, so this only
  // fires if something upstream changed — and saying so beats rendering a broken image element, which
  // reads to a user as "the app is broken" instead of "nothing was shown".
  if (!data.artifactId) {
    return (
      <p className="mb-2 text-[12px] text-muted-foreground" data-testid="media-missing">
        the agent reported media with no artifact id, so there is nothing to show
      </p>
    );
  }

  const src = `/api/palai/artifacts?id=${encodeURIComponent(data.artifactId)}`;
  const isVideo = (data.mediaType ?? "").startsWith("video/");

  return (
    <figure className="mb-2 overflow-hidden rounded-md border" data-testid="media" data-kind={isVideo ? "video" : "image"}>
      {isVideo ? (
        // controls, not autoplay: a recording that started playing by itself in a chat is a surprise,
        // and the user is being ASKED to look at this rather than shown it in passing.
        <video src={src} controls className="block max-h-[420px] w-full bg-black" data-testid="media-video" />
      ) : (
        // eslint-disable-next-line @next/next/no-img-element -- the bytes come from an authenticated
        // relay, not from a static origin the image optimiser can reach.
        <img
          src={src}
          alt={data.caption ?? data.path ?? "screenshot the agent took"}
          className="block max-h-[420px] w-full object-contain bg-muted"
          data-testid="media-image"
        />
      )}
      {/* The caption is the sentence the agent wrote, rendered as the agent's own words rather than as
          metadata — it is the "here is what changed" half of the question it is asking. */}
      <figcaption className="flex items-center justify-between gap-2 border-t px-3 py-2 text-[12px]">
        <span data-testid="media-caption">{data.caption?.trim() || "the agent showed this"}</span>
        {data.path ? (
          <span className="font-mono text-[11px] text-muted-foreground" data-testid="media-path">
            {data.path}
          </span>
        ) : null}
      </figcaption>
    </figure>
  );
}
