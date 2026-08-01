import { type Channel } from "@/lib/strip";

// THE LANE STRIP — this console's signature, and the only element in it that is drawn rather than laid out.
//
// WHY THIS AND NOT A LOGO. A console for watching autonomous agents work on machines an operator owns has
// exactly one characteristic artifact: a run unfolding in time. It is typed (eight lanes), it is timed (every
// frame carries a stamp), and it is append-only — and this console used to render it as rows sorted by date
// with the one true artifact, the transcript, a page deeper. The strip puts that artifact on every scale of
// the product: what a session did, what the whole list did, and what the deployment is doing now.
//
// IT ENCODES RATHER THAN DECORATES, which is the difference between this and a sparkline. Horizontal position
// is TIME, vertical position is TYPE, width is DURATION, and height is whether the frame failed. Nothing on
// it is chosen to look busy: a run with three frames draws three marks, and the empty space between them is
// the four minutes the operator spent waiting for a model.
//
// IT IS aria-hidden AND THAT IS NOT A SHORTCUT. A track of absolutely-positioned <span>s carries no text; a
// screen reader announcing it would read out thirty empty elements. Every reading it offers is also in words
// somewhere a screen reader DOES get — the caption below it, the Lane column of the table beside it, the
// Elapsed column, the counts in the gutter. The rule this file must never break is that removing the strip
// removes no information, only the ability to see it at a glance.
//
// app/globals.css §11 carries the geometry and the scales; this file carries the markup and nothing else.

export type StripScale = "stave" | "span" | "now";

export function LaneStrip({
  scale,
  channels,
  axis,
  caption,
  live,
  testId,
  captionTestId,
}: {
  scale: StripScale;
  channels: Channel[];
  /** The window's two ends, in words. A stave with no axis is a chart with no units. */
  axis?: [string, string];
  /** The sentence that carries everything the track shows, for the readers who cannot see it. */
  caption?: React.ReactNode;
  /**
   * True when the collection this strip drew still holds unfinished work.
   *
   * It is the ONLY thing that turns the console's one animation on, and it is a fact rather than a mood: a
   * pulse on a deployment where nothing is running would be the loudest false statement on the screen.
   */
  live?: boolean;
  testId?: string;
  captionTestId?: string;
}) {
  if (channels.length === 0) return null;
  return (
    <figure className="strip" data-scale={scale} data-live={live === true ? "true" : undefined} data-testid={testId}>
      <div className="strip-stave" aria-hidden="true">
        {channels.map((channel) => (
          <div className="strip-row" key={channel.lane + channel.label} data-lane={channel.lane} data-failure={channel.failure === true ? "true" : undefined}>
            {/* The gutter is the legend, in place. A separate legend under the track makes the reader carry
                eight colour/word pairs across a gap; a name at the head of its own channel does not. */}
            {scale === "span" ? null : <span className="strip-name">{channel.label}</span>}
            {scale === "span" ? null : <span className="strip-count">{channel.marks.length}</span>}
            <span className="strip-track">
              {channel.marks.map((mark) => (
                <span
                  key={mark.key}
                  className="strip-mark"
                  data-lane={channel.lane}
                  data-failure={mark.failure === true ? "true" : undefined}
                  data-span={mark.width === undefined ? undefined : "true"}
                  data-latest={mark.latest === true ? "true" : undefined}
                  style={
                    mark.width === undefined
                      ? { left: `${String(mark.at)}%` }
                      : { left: `${String(mark.at)}%`, width: `${String(mark.width)}%` }
                  }
                />
              ))}
            </span>
          </div>
        ))}
      </div>
      {axis === undefined ? null : (
        <div className="strip-axis" aria-hidden="true">
          <span>{axis[0]}</span>
          <span>{axis[1]}</span>
        </div>
      )}
      {caption === undefined ? null : <figcaption data-testid={captionTestId}>{caption}</figcaption>}
    </figure>
  );
}
