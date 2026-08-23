import { create, fromBinary, toBinary, type DescMessage, type MessageInitShape, type MessageShape } from "@bufbuild/protobuf"
import {
  CreateUploadTicketRequestSchema,
  CreateUploadTicketResponseSchema,
  CurrentUserRequestSchema,
  CurrentUserResponseSchema,
  ErrorSchema,
  LoginRequestSchema,
  LoginResponseSchema,
  type User,
} from "./gen/qol/v1/qol_pb"

const apiOrigin = import.meta.env.VITE_API_ORIGIN ?? "http://localhost:8080"

async function request<I extends DescMessage, O extends DescMessage>(path: string, inputSchema: I, input: MessageInitShape<I>, outputSchema: O): Promise<MessageShape<O>> {
  const response = await fetch(`${apiOrigin}${path}`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/protobuf" },
    body: toBinary(inputSchema, create(inputSchema, input)),
  })
  const bytes = new Uint8Array(await response.arrayBuffer())
  if (!response.ok) {
    const problem = fromBinary(ErrorSchema, bytes)
    throw new Error(problem.message || "Request failed")
  }
  return fromBinary(outputSchema, bytes)
}

export async function login(username: string, password: string): Promise<User> {
  const response = await request("/v1/login", LoginRequestSchema, { username, password }, LoginResponseSchema)
  if (!response.user) throw new Error("The API returned no user")
  return response.user
}

export async function currentUser(): Promise<User> {
  const response = await request("/v1/current-user", CurrentUserRequestSchema, {}, CurrentUserResponseSchema)
  if (!response.user) throw new Error("The API returned no user")
  return response.user
}

export async function uploadTicket(): Promise<string> {
  const response = await request("/v1/upload-ticket", CreateUploadTicketRequestSchema, {}, CreateUploadTicketResponseSchema)
  return response.ticket
}

export async function logout(): Promise<void> {
  const response = await fetch(`${apiOrigin}/v1/logout`, { method: "POST", credentials: "include" })
  if (!response.ok) throw new Error("Could not log out")
}

export function webTransportURL(ticket: string): string {
  const configured = import.meta.env.VITE_WEBTRANSPORT_ORIGIN ?? "https://localhost:4443"
  return `${configured}/v1/upload?ticket=${encodeURIComponent(ticket)}`
}
