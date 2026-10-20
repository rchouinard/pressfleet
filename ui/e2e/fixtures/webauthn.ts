// Chromium's virtual WebAuthn authenticator, over the Chrome DevTools
// Protocol: it answers the page's navigator.credentials prompts as a real
// security key or passkey would, with no user at the browser. It works on
// the browser server the suite connects to (connectOptions in
// playwright.config.ts): CDP sessions travel over the same connection.
//
// An authenticator belongs to one tab (the CDP session's target), so a
// spec adds it to the page it registers and signs in on, and keeps using
// that page. Specs get it from the `webauthn` fixture (index.ts), which
// removes it when the test ends.
import type { CDPSession, Page } from '@playwright/test'

/** What kind of authenticator to simulate. */
export type AuthenticatorKind
  /** A roaming USB key holding no credentials of its own (non-discoverable), without user verification, like a basic FIDO2 key. */
  = | 'security-key'
  /**
   * A platform passkey: discoverable (resident) credentials, user
   * verification, and backed up to the provider's account, like a synced
   * passkey on a phone or in a password manager.
   */
    | 'passkey'

/** A credential the virtual authenticator holds, as CDP reports it. */
export interface VirtualCredential {
  credentialId: string
  isResidentCredential: boolean
  rpId?: string
  userHandle?: string
  signCount: number
  backupEligibility?: boolean
  backupState?: boolean
}

/** The WebAuthn.addVirtualAuthenticator options the specs use. */
interface AuthenticatorOptions {
  protocol: 'ctap2'
  ctap2Version?: 'ctap2_1'
  transport: 'usb' | 'internal'
  hasResidentKey: boolean
  hasUserVerification: boolean
  isUserVerified?: boolean
  defaultBackupEligibility?: boolean
  defaultBackupState?: boolean
}

const options: Record<AuthenticatorKind, AuthenticatorOptions> = {
  'security-key': {
    protocol: 'ctap2',
    transport: 'usb',
    hasResidentKey: false,
    hasUserVerification: false,
  },
  'passkey': {
    protocol: 'ctap2',
    ctap2Version: 'ctap2_1',
    transport: 'internal',
    hasResidentKey: true,
    hasUserVerification: true,
    isUserVerified: true,
    defaultBackupEligibility: true,
    defaultBackupState: true,
  },
}

/** A virtual authenticator attached to one page. */
export class VirtualAuthenticator {
  private constructor(private readonly cdp: CDPSession, readonly id: string) {}

  /** Adds an authenticator of the kind to the page's tab. */
  static async attach(page: Page, kind: AuthenticatorKind): Promise<VirtualAuthenticator> {
    const cdp = await page.context().newCDPSession(page)
    await cdp.send('WebAuthn.enable')
    const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
      // Presence (the touch) is simulated, so prompts answer at once.
      options: { ...options[kind], automaticPresenceSimulation: true },
    })
    return new VirtualAuthenticator(cdp, authenticatorId)
  }

  /** The credentials it holds. */
  async credentials(): Promise<VirtualCredential[]> {
    const { credentials } = await this.cdp.send('WebAuthn.getCredentials', { authenticatorId: this.id })
    return credentials as VirtualCredential[]
  }

  /** Removes the authenticator and closes its CDP session. */
  async detach(): Promise<void> {
    await this.cdp.send('WebAuthn.removeVirtualAuthenticator', { authenticatorId: this.id }).catch(() => undefined)
    await this.cdp.detach().catch(() => undefined)
  }
}
