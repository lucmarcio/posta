<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { adminApi } from "../../api/admin";
import { useNotificationStore } from "../../stores/notification";
import { useConfirm } from "../../composables/useConfirm";
import { apiMessage } from "../../composables/apiError";
import type { AdminDomainDetail, DnsRecord } from "../../api/types";

const route = useRoute();
const router = useRouter();
const notify = useNotificationStore();
const { confirm } = useConfirm();

const id = Number(route.params.id);
const domain = ref<AdminDomainDetail | null>(null);
const loading = ref(true);
const loadFailed = ref(false);
const verifying = ref(false);
const overriding = ref(false);

const showOverride = ref(false);
const overrideReason = ref("");

async function load() {
  loading.value = true;
  try {
    domain.value = (await adminApi.getDomain(id)).data.data;
  } catch (e: any) {
    loadFailed.value = true;
    notify.error(apiMessage(e, "Failed to load the domain"));
  } finally {
    loading.value = false;
  }
}

async function runVerification() {
  verifying.value = true;
  try {
    const res = await adminApi.verifyDomain(id);
    const result = res.data.data;
    await load();
    if (result.verification.ownership_verified) {
      notify.success("DNS verification passed");
    } else if (result.conflict_workspace_id) {
      notify.error(
        `DNS is correct, but ${result.conflict_workspace_name || "another workspace"} already holds this domain verified`
      );
    } else {
      notify.info("DNS verification ran; ownership is still unproven");
    }
  } catch (e: any) {
    notify.error(apiMessage(e, "Verification failed"));
  } finally {
    verifying.value = false;
  }
}

async function grantOwnership() {
  if (!overrideReason.value.trim()) return;
  overriding.value = true;
  try {
    await adminApi.setDomainVerification(id, true, overrideReason.value.trim());
    showOverride.value = false;
    overrideReason.value = "";
    await load();
    notify.success("Ownership granted");
  } catch (e: any) {
    notify.error(apiMessage(e, "Could not grant ownership"));
  } finally {
    overriding.value = false;
  }
}

async function revokeOwnership() {
  const confirmed = await confirm({
    title: "Revoke ownership",
    message:
      `Mail from ${domain.value?.domain} will be treated as unverified. ` +
      "Workspaces that require a verified domain will stop sending from it.",
    confirmText: "Revoke",
    variant: "danger",
  });
  if (!confirmed) return;
  overriding.value = true;
  try {
    await adminApi.setDomainVerification(id, false, "");
    await load();
    notify.success("Ownership revoked");
  } catch (e: any) {
    notify.error(apiMessage(e, "Could not revoke ownership"));
  } finally {
    overriding.value = false;
  }
}

const checks = computed(() => {
  const d = domain.value;
  if (!d) return [];
  return [
    { key: "ownership", label: "Ownership", passed: d.ownership_verified, record: d.records?.verification },
    { key: "spf", label: "SPF", passed: d.spf_verified, record: d.records?.spf },
    { key: "dkim", label: "DKIM", passed: d.dkim_verified, record: d.records?.dkim },
    { key: "dmarc", label: "DMARC", passed: d.dmarc_verified, record: d.records?.dmarc },
  ];
});

const hasConflict = computed(() => !!domain.value?.conflict_workspace_id);

function copy(record?: DnsRecord) {
  if (!record) return;
  navigator.clipboard?.writeText(record.value);
  notify.success("Record value copied");
}

function formatDate(value?: string) {
  return value ? new Date(value).toLocaleString() : "—";
}

onMounted(load);
</script>

<template>
  <div>
    <div class="page-header">
      <div>
        <h1>{{ domain?.domain || "Domain" }}</h1>
        <p v-if="domain" class="page-subtitle">
          {{ domain.workspace_name || (domain.workspace_id ? `Workspace #${domain.workspace_id}` : "No workspace") }}
          · added {{ formatDate(domain.created_at) }}
        </p>
      </div>
      <div class="header-actions">
        <button class="btn btn-secondary" :disabled="verifying" @click="runVerification">
          {{ verifying ? "Checking DNS…" : "Run DNS check" }}
        </button>
        <button
          v-if="domain && !domain.ownership_verified"
          class="btn btn-primary"
          :disabled="hasConflict"
          :title="hasConflict ? 'Another workspace holds this domain verified' : ''"
          @click="showOverride = true"
        >
          Mark verified
        </button>
        <button
          v-else-if="domain"
          class="btn btn-danger"
          :disabled="overriding"
          @click="revokeOwnership"
        >
          Revoke ownership
        </button>
        <button class="btn btn-secondary" @click="router.push('/admin/domains')">Back</button>
      </div>
    </div>

    <div v-if="loading" class="loading-page"><div class="spinner"></div></div>

    <div v-else-if="loadFailed || !domain" class="card">
      <div class="empty-state">
        <h3>Domain not found</h3>
        <button class="btn btn-secondary" @click="router.push('/admin/domains')">Back to domains</button>
      </div>
    </div>

    <template v-else>
      <div v-if="hasConflict" class="alert-banner">
        <strong>Already verified elsewhere.</strong>
        <span>
          {{ domain.conflict_workspace_name || `Workspace #${domain.conflict_workspace_id}` }}
          holds <code>{{ domain.domain }}</code> verified. Only one workspace may own a domain, so
          ownership cannot be granted here until it is revoked there.
        </span>
      </div>

      <div class="card" style="margin-bottom: 20px">
        <div class="card-header"><h2>Status</h2></div>
        <div class="card-body">
          <div class="checks">
            <div v-for="check in checks" :key="check.key" class="check" :class="{ passed: check.passed }">
              <span class="check-mark" aria-hidden="true">
                <svg v-if="check.passed" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M3.5 8.5l3 3 6-7" />
                </svg>
              </span>
              <span class="check-label">{{ check.label }}</span>
              <span class="check-state">{{ check.passed ? "passing" : "not detected" }}</span>
            </div>
          </div>
          <p class="note">
            Ownership decides whether the workspace may send from this domain at all. SPF, DKIM and
            DMARC do not gate sending; they decide how the mail is received.
          </p>
        </div>
      </div>

      <div class="card" style="margin-bottom: 20px">
        <div class="card-header">
          <h2>Owner</h2>
        </div>
        <div class="card-body">
          <table>
            <tbody>
              <tr>
                <td class="label-cell">Workspace</td>
                <td>{{ domain.workspace_name || (domain.workspace_id ? `#${domain.workspace_id}` : "—") }}</td>
              </tr>
              <tr>
                <td class="label-cell">Account</td>
                <td>{{ domain.owner_email || `#${domain.owner_id}` }}</td>
              </tr>
              <tr>
                <td class="label-cell">Verification token</td>
                <td><code>{{ domain.verification_token }}</code></td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div class="card">
        <div class="card-header"><h2>Required DNS records</h2></div>
        <div class="card-body">
          <p class="note" style="margin-top: 0">
            What the owner has to publish. Values are shown so you can compare them against what
            their zone actually returns.
          </p>
          <div v-for="check in checks" :key="check.key" class="record">
            <div class="record-head">
              <span class="record-label">{{ check.label }}</span>
              <span :class="check.passed ? 'badge badge-success' : 'badge badge-neutral'">
                {{ check.passed ? "found" : "missing" }}
              </span>
              <button v-if="check.record" class="record-copy" type="button" @click="copy(check.record)">
                Copy value
              </button>
            </div>
            <dl v-if="check.record" class="record-body">
              <dt>Type</dt>
              <dd>{{ check.record.type }}</dd>
              <dt>Name</dt>
              <dd>{{ check.record.host }}</dd>
              <dt>Value</dt>
              <dd class="wrap">{{ check.record.value }}</dd>
            </dl>
            <p v-else class="note">No record is published for this check.</p>
          </div>
        </div>
      </div>
    </template>

    <div v-if="showOverride" class="modal-overlay" @click.self="showOverride = false">
      <div class="modal">
        <div class="modal-header"><h3>Mark {{ domain?.domain }} verified</h3></div>
        <div class="modal-body">
          <p class="note" style="margin-top: 0">
            This grants ownership without a DNS check, and the workspace will be able to send from
            the domain immediately. Use it when DNS cannot settle the question — a registrar that
            strips TXT records, an internal domain with no public zone — not as a shortcut past a
            failing check.
          </p>
          <div class="form-group">
            <label class="form-label" for="override-reason">Reason</label>
            <input
              id="override-reason"
              v-model="overrideReason"
              class="form-input"
              placeholder="Why is the DNS proof being bypassed?"
            />
            <span class="form-hint">Recorded on the audit entry, alongside who granted it.</span>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" @click="showOverride = false">Cancel</button>
          <button
            class="btn btn-primary"
            :disabled="overriding || !overrideReason.trim()"
            @click="grantOwnership"
          >
            {{ overriding ? "Granting…" : "Grant ownership" }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.page-subtitle {
  font-size: 13px;
  color: var(--text-tertiary);
  margin-top: 4px;
}

.header-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.alert-banner {
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 12px 16px;
  margin-bottom: 20px;
  border-radius: 10px;
  border: 1px solid var(--warning-500);
  background: var(--warning-50);
  font-size: 13px;
  color: var(--text-secondary);
}

.alert-banner strong {
  color: var(--text-primary);
}

.checks {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 14px;
}

.check {
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 12px 14px;
  border: 1px solid var(--border-primary);
  border-radius: 8px;
}

.check-mark {
  width: 20px;
  height: 20px;
  border-radius: 50%;
  border: 1.5px solid var(--border-input);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--text-inverse);
}

.check-mark svg {
  width: 12px;
  height: 12px;
}

.passed .check-mark {
  background: var(--success-600);
  border-color: var(--success-600);
}

.check-label {
  font-size: 14px;
  font-weight: 600;
  color: var(--text-primary);
}

.check-state {
  font-size: 12px;
  color: var(--text-tertiary);
}

.note {
  font-size: 12px;
  color: var(--text-tertiary);
  margin-top: 14px;
  line-height: 1.6;
}

.label-cell {
  font-weight: 600;
  width: 190px;
  color: var(--text-secondary);
}

.record {
  padding: 14px 0;
  border-bottom: 1px solid var(--border-secondary);
}

.record:last-child {
  border-bottom: 0;
}

.record-head {
  display: flex;
  align-items: center;
  gap: 10px;
}

.record-label {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary);
}

.record-copy {
  margin-left: auto;
  border: 0;
  background: none;
  padding: 0;
  font: inherit;
  font-size: 12px;
  color: var(--text-tertiary);
  cursor: pointer;
}

.record-copy:hover {
  color: var(--primary-600);
  text-decoration: underline;
}

.record-body {
  display: grid;
  grid-template-columns: 70px 1fr;
  gap: 4px 12px;
  margin: 10px 0 0;
  font-size: 13px;
}

.record-body dt {
  color: var(--text-tertiary);
  font-size: 12px;
}

.record-body dd {
  margin: 0;
  color: var(--text-primary);
  font-family: "SFMono-Regular", Consolas, monospace;
  font-size: 12px;
}

.wrap {
  overflow-wrap: anywhere;
}
</style>
