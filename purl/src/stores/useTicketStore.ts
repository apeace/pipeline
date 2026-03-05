import type { AppTicketCommentRow, AppTicketRow } from "@purl/lib"
import { getOrg, getTickets, getTicketsByTicketIdComments } from "@purl/lib"
import { defineStore } from "pinia"
import { computed, reactive, ref } from "vue"
import type { CallData, CommChannel, MergeData, MessageType, VoicemailData } from "../utils/parseComment"
import { parseComment, sanitizeHtml, stripHtml } from "../utils/parseComment"

// ── Types ───────────────────────────────────────────────

export interface Message {
  id: number
  from: string
  channel: string
  time: string
  text: string
  htmlBody?: string
  authorName?: string
  automated?: boolean
  messageType?: MessageType
  commChannel?: CommChannel
  call?: CallData
  voicemail?: VoicemailData
  merge?: MergeData
  transcript?: string
  hasRecording?: boolean
  commentId?: string
  type?: string
  recording?: {
    duration: number
    waveform: number[]
    transcript: { speaker: string; time: string; text: string }[]
  }
}

export interface Ticket {
  id: string
  name: string
  company: string
  ticketId: string
  zendeskTicketId?: number
  subject: string
  description: string
  createdAt: string
  // ISO timestamp of the customer's most recent unanswered message, or undefined if the agent
  // has already replied. Used to sort by longest waiting.
  customerWaitingSince?: string
  // ISO timestamp of the customer's most recent message regardless of agent reply status.
  // Used to sort by most recent customer activity.
  lastCustomerReplyAt?: string
  // ISO timestamp of when the ticket was first marked solved or closed. Undefined if not yet resolved.
  resolvedAt?: string
  wait: string
  avatarColor: string
  status: string
  read: boolean
  starred: boolean
  labels: string[]
  time: string
  email: string
  phone: string
  subscription: { status: string; id: string; plan: string }
  tags: string[]
  temperature: string
  // AI-generated fields. aiTitle is set once and never overwritten; summary/temperature update on each reply.
  aiTitle?: string
  aiSummary?: string
  aiTemperature?: number
  assignee: string
  notes: string
  messages: Message[]
  ticketHistory: { time: string; event: string }[]
  subscriberHistory: { ticketId: string; status: string; subject: string; date: string }[]
}

// ── Module-level helpers (exported for direct import at call sites) ──

const STATUS_ORDER: Record<string, number> = { escalated: 0, new: 1, open: 2, pending: 3, solved: 4, closed: 5 }

const AVATAR_COLORS = [
  "#6366f1", "#ec4899", "#34d399", "#f59e0b",
  "#3b82f6", "#a855f7", "#ef4444", "#14b8a6",
]

export function avatarColor(name: string): string {
  let hash = 0
  for (const ch of name) hash = (hash * 31 + ch.charCodeAt(0)) & 0xffff
  return AVATAR_COLORS[hash % AVATAR_COLORS.length]
}

/** Returns the epoch ms of the customer's most recent message, or 0 if none. Used to sort by most recent activity. */
export function lastCustomerReplyMs(ticket: Ticket): number {
  if (!ticket.lastCustomerReplyAt) return 0
  return new Date(ticket.lastCustomerReplyAt).getTime()
}

/** Returns how many minutes a customer has been waiting for a reply, or 0 if the agent has replied. */
export function waitingMinutes(ticket: Ticket): number {
  if (!ticket.customerWaitingSince) return 0
  return Math.floor((Date.now() - new Date(ticket.customerWaitingSince).getTime()) / 60000)
}

export function parseWait(str: string): number {
  let mins = 0
  const d = str.match(/(\d+)d/)
  const h = str.match(/(\d+)h/)
  const m = str.match(/(\d+)m/)
  if (d) mins += parseInt(d[1]) * 1440
  if (h) mins += parseInt(h[1]) * 60
  if (m) mins += parseInt(m[1])
  return mins
}

function formatWait(createdAt: string): string {
  const mins = Math.floor((Date.now() - new Date(createdAt).getTime()) / 60000)
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  const rem = mins % 60
  return rem ? `${hrs}h ${rem}m` : `${hrs}h`
}

function formatTime(createdAt: string): string {
  return new Date(createdAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
}

// Extended type until the OpenAPI client regenerates with new fields
type TicketRowExt = AppTicketRow & {
  zendesk_ticket_id?: number
  assignee_name?: string
  reporter_email?: string
}

function toTicket(raw: AppTicketRow): Ticket {
  const t = raw as TicketRowExt
  const id = t.id ?? ""
  const reporterName = t.reporter_name ?? ""
  const receivedAt = t.received_at ?? ""
  const zendeskId = t.zendesk_ticket_id
  return {
    id,
    name: reporterName,
    company: "",
    ticketId: zendeskId ? `#${zendeskId}` : `#${id.slice(0, 6).toUpperCase()}`,
    zendeskTicketId: zendeskId ?? undefined,
    subject: t.title ?? "",
    description: stripHtml(t.description ?? ""),
    createdAt: receivedAt,
    customerWaitingSince: t.customer_waiting_since ?? undefined,
    lastCustomerReplyAt: t.last_customer_reply_at ?? undefined,
    resolvedAt: t.resolved_at ?? undefined,
    wait: formatWait(receivedAt),
    avatarColor: avatarColor(reporterName),
    status: t.zendesk_status ?? "",
    read: false,
    starred: false,
    labels: [],
    time: formatTime(receivedAt),
    email: t.reporter_email ?? "",
    phone: "",
    subscription: { status: "active", id: "", plan: "" },
    tags: [],
    temperature: "warm",
    // Filter out the example title that the LLM sometimes echoes from the prompt.
    aiTitle: t.ai_title === "Login issue with SSO" ? undefined : (t.ai_title ?? undefined),
    aiSummary: t.ai_summary ?? undefined,
    aiTemperature: t.ai_temperature ?? undefined,
    assignee: t.assignee_name ?? "Unassigned",
    notes: "",
    messages: [],
    ticketHistory: [
      { time: formatWait(receivedAt), event: "Ticket created" },
    ],
    subscriberHistory: [],
  }
}

// ── Store ────────────────────────────────────────────────

export const useTicketStore = defineStore("tickets", () => {
  // ── State ─────────────────────────────────────────────

  const tickets = ref<Ticket[]>([])
  const zendeskSubdomain = ref("")


  // ── Filters ─────────────────────────────────────────────

  const filterKeyword = ref("")
  const filterAssignees = reactive(new Set<string>())
  const filterStatuses = reactive(new Set<string>())

  const filteredTickets = computed(() => {
    let result = tickets.value

    const kw = filterKeyword.value.trim().toLowerCase()
    if (kw) {
      result = result.filter((t) => {
        const firstMsg = t.messages[0]?.text ?? ""
        return t.subject.toLowerCase().includes(kw) || firstMsg.toLowerCase().includes(kw)
      })
    }

    if (filterAssignees.size) {
      result = result.filter((t) => filterAssignees.has(t.assignee))
    }
    if (filterStatuses.size) {
      result = result.filter((t) => filterStatuses.has(t.status))
    }

    return result
  })

  const activeFilterCount = computed(() => {
    let n = 0
    if (filterKeyword.value.trim()) n++
    if (filterAssignees.size) n++
    if (filterStatuses.size) n++
    return n
  })

  const uniqueAssignees = computed(() =>
    [...new Set(tickets.value.map((t) => t.assignee))].sort()
  )

  function clearFilters() {
    filterKeyword.value = ""
    filterAssignees.clear()
    filterStatuses.clear()
  }

  // ── Sorting ─────────────────────────────────────────────

  const sortBy = ref("time")

  const sortedTickets = computed(() => {
    const list = [...filteredTickets.value]
    if (sortBy.value === "status") {
      list.sort((a, b) => (STATUS_ORDER[a.status] ?? 9) - (STATUS_ORDER[b.status] ?? 9))
    } else if (sortBy.value === "assignee") {
      list.sort((a, b) => a.assignee.localeCompare(b.assignee))
    }
    return list
  })

  // ── Derived state ───────────────────────────────────────

  const openTickets = computed(() => tickets.value.filter((t) => t.status === "new" || t.status === "open"))

  const hudOpen = computed(() => openTickets.value.length)

  const hudWaiting = computed(() => openTickets.value.length)

  const hudResolvedToday = computed(() => {
    const startOfToday = new Date()
    startOfToday.setHours(0, 0, 0, 0)
    return tickets.value.filter((t) => t.resolvedAt && new Date(t.resolvedAt) >= startOfToday).length
  })

  const hudLongestWait = computed(() => {
    let maxMins = 0
    for (const t of openTickets.value) {
      const mins = waitingMinutes(t)
      if (mins > maxMins) maxMins = mins
    }
    if (maxMins < 60) return `${maxMins}m`
    const hrs = Math.floor(maxMins / 60)
    const rem = maxMins % 60
    return rem ? `${hrs}h ${rem}m` : `${hrs}h`
  })

  // ── Mutations ───────────────────────────────────────────

  // TODO: persist status change via API
  function resolveTicket(id: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (!ticket || ticket.status === "solved" || ticket.status === "closed") return
    ticket.status = "solved"
    ticket.resolvedAt = new Date().toISOString()
    ticket.read = true
  }

  // TODO: persist status change via API
  function archiveTicket(id: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.status = "closed"
  }

  // TODO: persist deletion via API
  function deleteTicket(id: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.status = "closed"
  }

  // TODO: persist read state via API
  function markRead(id: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.read = true
  }

  // TODO: persist starred state via API
  function toggleStar(id: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.starred = !ticket.starred
  }

  // TODO: send reply via API
  function sendReply(id: string, text: string, channel = "email") {
    const ticket = tickets.value.find((t) => t.id === id)
    if (!ticket) return
    ticket.messages.push({
      id: ticket.messages.length + 1,
      from: "agent",
      channel,
      time: "just now",
      text,
    })
    ticket.read = true
  }

  // TODO: persist status change via API
  function setStatus(id: string, status: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.status = status
  }

  // TODO: persist assignee change via API
  function setAssignee(id: string, assignee: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.assignee = assignee
  }

  // TODO: persist temperature change via API
  function setTemperature(id: string, temperature: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.temperature = temperature
  }

  // TODO: persist tag addition via API
  function addTag(id: string, tag: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket && !ticket.tags.includes(tag)) ticket.tags.push(tag)
  }

  // TODO: persist tag removal via API
  function removeTag(id: string, tag: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.tags = ticket.tags.filter((t) => t !== tag)
  }

  // TODO: persist notes via API
  function updateNotes(id: string, text: string) {
    const ticket = tickets.value.find((t) => t.id === id)
    if (ticket) ticket.notes = text
  }

  // ── Data fetching ────────────────────────────────────────

  const ticketsLoaded = ref(false)

  let loadPromise: Promise<void> | null = null

  function loadTickets() {
    if (!loadPromise) {
      loadPromise = Promise.all([
        getTickets().then(({ data }) => {
          if (data) tickets.value = data.map(toTicket)
        }),
        getOrg().then(({ data }) => {
          if (data?.zendesk_subdomain) zendeskSubdomain.value = data.zendesk_subdomain
        }),
      ]).then(() => {
        ticketsLoaded.value = true
      })
    }
    return loadPromise
  }

  function reloadTickets() {
    loadPromise = null
    ticketsLoaded.value = false
    return loadTickets()
  }

  // Track which ticket IDs have had comments loaded to avoid duplicate fetches
  const loadedCommentTickets = new Set<string>()

  // Extended type until the OpenAPI client regenerates with new fields
  type CommentRowExt = AppTicketCommentRow & {
    author_name?: string
    author_display_name?: string
    html_body?: string
    call_id?: number
    has_recording?: boolean
    transcription_text?: string
    transcription_status?: string
    call_duration?: number
    call_from?: string
    call_to?: string
    answered_by_name?: string
    call_location?: string
    call_started_at?: string
  }

  function commentToMessage(raw: AppTicketCommentRow, index: number): Message {
    const c = raw as CommentRowExt
    const body = c.body ?? ""
    const channel = c.channel ?? "email"
    const role = c.role ?? "customer"
    const parsed = parseComment(body, channel, role)

    // Use the full HTML body from Zendesk when available (preserves tables, lists, etc.)
    const rawHtml = c.html_body
    const htmlBody = rawHtml ? sanitizeHtml(rawHtml) : undefined

    // When Zendesk Automation wraps an agent reply, the body starts with
    // "(HH:MM:SS) Agent Name: ..." — extract the real sender and clean body.
    // Prefer author_display_name (parsed speaker from web chat transcripts).
    let displayAuthor = c.author_display_name || c.author_name || undefined
    let displayText = parsed.cleanBody
    let displayHtml = htmlBody
    let automated = false
    const authorName = c.author_name ?? ""
    if (/automation|system/i.test(authorName) && parsed.messageType !== "web_chat") {
      const wrapped = parsed.cleanBody.match(/^\(\d{1,2}:\d{2}:\d{2}\)\s+(.+?):\s*([\s\S]*)$/)
      if (wrapped) {
        displayAuthor = wrapped[1].trim()
        displayText = wrapped[2].trim()
        // Clear htmlBody so the cleaned text is used instead
        displayHtml = undefined
        automated = true
      }
    }

    const msg: Message = {
      id: index,
      from: role === "agent" ? "agent" : "customer",
      channel,
      time: formatTime(c.received_at ?? ""),
      text: displayText,
      htmlBody: displayHtml,
      authorName: displayAuthor,
      automated,
      messageType: parsed.messageType,
      commChannel: parsed.commChannel,
      call: parsed.call,
      voicemail: parsed.voicemail,
      merge: parsed.merge,
    }

    // Attach transcript text — may come from DB (call_id) or from a paired
    // Zendesk Automation comment (transcription_text set by pairTranscriptComments)
    if (c.transcription_text) {
      msg.transcript = c.transcription_text
    }

    // Prefer structured voice data from DB when available
    if (c.call_id) {
      msg.commentId = c.id ?? undefined
      msg.hasRecording = c.has_recording ?? false

      // Overlay structured fields onto regex-parsed call/voicemail data
      if (msg.call) {
        if (c.call_from) msg.call.callFrom = c.call_from
        if (c.call_to) msg.call.callTo = c.call_to
        if (c.call_duration) msg.call.duration = formatCallDuration(c.call_duration)
        if (c.answered_by_name) msg.call.agentName = c.answered_by_name
        if (c.call_location) msg.call.location = c.call_location
      } else if (msg.voicemail) {
        if (c.call_from) msg.voicemail.callFrom = c.call_from
        if (c.call_to) msg.voicemail.callTo = c.call_to
        if (c.call_duration) msg.voicemail.duration = formatCallDuration(c.call_duration)
        if (c.call_location) msg.voicemail.location = c.call_location
      }
    }

    return msg
  }

  function formatCallDuration(seconds: number): string {
    const mins = Math.floor(seconds / 60)
    const secs = seconds % 60
    if (mins === 0) return `${secs}s`
    return secs ? `${mins}m ${secs}s` : `${mins}m`
  }

  async function loadComments(ticketId: string) {
    if (loadedCommentTickets.has(ticketId)) return
    loadedCommentTickets.add(ticketId)

    const { data } = await getTicketsByTicketIdComments({ path: { ticketID: ticketId } })
    if (!data) return

    const ticket = tickets.value.find((t) => t.id === ticketId)
    if (!ticket) return

    // Deduplicate comments that share the same call_id, then match "Call to/from"
    // summaries with their detailed "Inbound/Outbound call" counterparts by phone
    // number, then pair Zendesk Automation transcript comments with the adjacent
    // call/voicemail so everything renders in a single card.
    const deduped = deduplicateByCallId(data)
    const merged = deduplicateCallSummaries(deduped)
    const paired = pairTranscriptComments(merged)
    ticket.messages = paired.map((c, i) => commentToMessage(c, i + 1))
  }

  function isCallOrVoicemail(body: string): boolean {
    return /^(Inbound|Outbound) call/.test(body)
      || /^Voicemail from/.test(body)
      || /^Call (to|from):/.test(body)
  }

  /** Extract clean transcript text from a Zendesk Automation transcript body. */
  function extractTranscriptText(body: string): string {
    const clean = stripHtml(body)
    // Remove markdown heading + "Call transcript:" prefix
    let text = clean.replace(/^#+\s*\*{0,2}\s*Call transcript:?\s*\*{0,2}\s*/i, "")
    // Strip markdown bold markers
    text = text.replace(/\*\*/g, "")
    // Remove Zendesk disclaimer after *** separator
    text = text.replace(/\*\s*\*\s*\*[\s\S]*$/, "").trim()
    // Insert line breaks before each timestamp (e.g. "00:20 Customer ...")
    text = text.replace(/\s+(\d{2}:\d{2}\s)/g, "\n$1")
    return text.trim()
  }

  /**
   * Zendesk posts call transcripts as separate internal-note comments from
   * "Zendesk Automation". Detect these and attach their text to the nearest
   * preceding call/voicemail comment, then remove the standalone transcript.
   */
  function pairTranscriptComments(comments: AppTicketCommentRow[]): AppTicketCommentRow[] {
    const absorbed = new Set<number>()

    for (let i = 0; i < comments.length; i++) {
      const ext = comments[i] as CommentRowExt
      const body = stripHtml(ext.body ?? "")
      if (!/call transcript/i.test(body)) continue

      // Walk backwards to find the nearest call/voicemail
      for (let j = i - 1; j >= 0; j--) {
        if (absorbed.has(j)) continue
        const prev = comments[j] as CommentRowExt
        const prevBody = stripHtml(prev.body ?? "")
        if (isCallOrVoicemail(prevBody) || prev.call_id) {
          if (!prev.transcription_text) {
            prev.transcription_text = extractTranscriptText(ext.body ?? "")
          }
          absorbed.add(i)
          break
        }
      }
    }

    return comments.filter((_, i) => !absorbed.has(i))
  }

  function deduplicateByCallId(comments: AppTicketCommentRow[]): AppTicketCommentRow[] {
    const seen = new Map<number, number>() // call_id → index in result
    const result: AppTicketCommentRow[] = []

    for (const c of comments) {
      const ext = c as CommentRowExt
      if (!ext.call_id) {
        result.push(c)
        continue
      }

      const existing = seen.get(ext.call_id)
      if (existing === undefined) {
        seen.set(ext.call_id, result.length)
        result.push(c)
      } else {
        // Keep whichever comment is richer (has recording, or is a full
        // inbound/outbound call rather than a summary)
        const prev = result[existing] as CommentRowExt
        const prevIsDetailed = /^(Inbound|Outbound) call/.test(prev.body ?? "")
        const curIsDetailed = /^(Inbound|Outbound) call/.test(ext.body ?? "")

        if (!prevIsDetailed && curIsDetailed) {
          // Current is richer — replace, carrying over recording/transcript if missing
          if (!ext.has_recording && prev.has_recording) ext.has_recording = prev.has_recording
          if (!ext.transcription_text && prev.transcription_text) ext.transcription_text = prev.transcription_text
          result[existing] = c
        } else {
          // Previous is richer — carry over any data the prev lacks
          if (!prev.has_recording && ext.has_recording) prev.has_recording = ext.has_recording
          if (!prev.transcription_text && ext.transcription_text) prev.transcription_text = ext.transcription_text
        }
      }
    }

    return result
  }

  /** Extract digit-only phone numbers from text for fuzzy matching. */
  function extractPhoneDigits(text: string): string[] {
    return [...text.matchAll(/\+?[\d()\s-]{7,}/g)]
      .map((m) => m[0].replace(/\D/g, ""))
      .filter((d) => d.length >= 7)
  }

  /**
   * Zendesk sometimes creates a short "Call to/from:" summary AND a detailed
   * "Inbound/Outbound call" comment for the same call without sharing a call_id.
   * Match them by phone number and absorb the summary into the detailed version.
   */
  function deduplicateCallSummaries(comments: AppTicketCommentRow[]): AppTicketCommentRow[] {
    const absorbed = new Set<number>()

    for (let i = 0; i < comments.length; i++) {
      if (absorbed.has(i)) continue
      const body = stripHtml((comments[i] as CommentRowExt).body ?? "")
      // Only target the shorter "Call to/from:" summaries
      if (!/^Call (to|from):/.test(body)) continue

      const phones = extractPhoneDigits(body)
      if (!phones.length) continue

      // Search nearby comments (within 5 positions) for a detailed match
      for (let j = Math.max(0, i - 5); j < Math.min(comments.length, i + 6); j++) {
        if (j === i || absorbed.has(j)) continue
        const otherBody = stripHtml((comments[j] as CommentRowExt).body ?? "")
        if (!/^(Inbound|Outbound) call/.test(otherBody)) continue

        const otherPhones = extractPhoneDigits(otherBody)
        if (phones.some((p) => otherPhones.includes(p))) {
          absorbed.add(i)
          break
        }
      }
    }

    return comments.filter((_, i) => !absorbed.has(i))
  }

  // Auto-load on first store access
  loadTickets()

  return {
    activeFilterCount,
    addTag,
    archiveTicket,
    clearFilters,
    deleteTicket,
    filterAssignees,
    filterKeyword,
    filterStatuses,
    filteredTickets,
    hudLongestWait,
    hudOpen,
    hudResolvedToday,
    hudWaiting,
    loadComments,
    loadTickets,
    markRead,
    openTickets,
    reloadTickets,
    removeTag,
    resolveTicket,
    sendReply,
    setAssignee,
    setStatus,
    setTemperature,
    sortBy,
    sortedTickets,
    ticketsLoaded,
    zendeskSubdomain,
    tickets,
    toggleStar,
    uniqueAssignees,
    updateNotes,
  }
})
