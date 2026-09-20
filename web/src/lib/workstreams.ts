import { fetchJSON, postJSON } from './api';

export interface VerificationPolicy {
  commands?: string[];
  requiredSignals?: string[];
  maxRepairAttempts?: number;
}

export interface VerificationResult {
  status?: string;
  source?: string;
  summary?: string;
  updatedAt?: string;
}

export interface Goal {
  objective?: string;
  acceptanceCriteria?: string[];
  constraints?: string[];
  definitionOfDone?: string[];
  verificationPolicy?: VerificationPolicy;
}

export interface Workstream {
  id: string;
  title: string;
  workspace: string;
  status: 'active' | 'blocked' | 'completed' | 'archived';
  summary?: string;
  nextAction?: string;
  decisions?: string[];
  openQuestions?: string[];
  tags?: string[];
  goal?: Goal;
  lastVerification?: VerificationResult;
  createdAt: string;
  updatedAt: string;
}

export interface TimelineEvent {
  id: string;
  type: string;
  at: string;
  message?: string;
  data?: Record<string, string>;
}

export interface WorkstreamPlanDefinition {
  name?: string;
  objective?: string;
  verifyCommand?: string;
  stages?: Array<{ id: string; name: string; kind?: string; taskIds?: string[] }>;
  tasks?: Array<{ id: string; name: string; stage?: string; acceptanceCriteria?: string[] }>;
}

export interface WorkstreamPlanStageAttempt {
  runId: string;
  planRevision: number;
  status: string;
  verificationStatus?: string;
  receiptDigest?: string;
  evidence?: {
    source: string;
    receiptDigest: string;
    criteriaDigest: string;
    verificationStatus: string;
    completionStatus: string;
    recordedAt: string;
  };
}

export interface WorkstreamPlanStage {
  id: string;
  status: 'pending' | 'running' | 'completed' | 'failed' | string;
  attempts?: WorkstreamPlanStageAttempt[];
}

export interface WorkstreamPlan {
  id: string;
  workstreamId: string;
  revision: number;
  stateRevision: number;
  status: 'draft' | 'approved' | 'executing' | 'completed' | 'failed' | string;
  approvedRevision?: number;
  definition: WorkstreamPlanDefinition;
  stages: WorkstreamPlanStage[];
}

function workstreamPlanPath(workstreamId: string, workDir?: string): string {
  const params = workDir ? `?workDir=${encodeURIComponent(workDir)}` : '';
  return `/api/workstreams/${encodeURIComponent(workstreamId)}/plans${params}`;
}

export async function listWorkstreamPlans(workstreamId: string, workDir?: string): Promise<WorkstreamPlan[]> {
  const data = await fetchJSON<{ plans: WorkstreamPlan[] }>(workstreamPlanPath(workstreamId, workDir));
  return data.plans ?? [];
}

export async function getWorkstreamPlan(workstreamId: string, planId: string, workDir?: string): Promise<WorkstreamPlan> {
  const params = workDir ? `?workDir=${encodeURIComponent(workDir)}` : '';
  const data = await fetchJSON<{ plan: WorkstreamPlan }>(
    `/api/workstreams/${encodeURIComponent(workstreamId)}/plans/${encodeURIComponent(planId)}${params}`,
  );
  return data.plan;
}

export async function approveWorkstreamPlan(
  workstreamId: string,
  planId: string,
  workDir: string,
  expectedRevision: number,
  expectedStateRevision: number,
): Promise<WorkstreamPlan> {
  const data = await postJSON<{ plan: WorkstreamPlan }>(
    `/api/workstreams/${encodeURIComponent(workstreamId)}/plans/${encodeURIComponent(planId)}/approve`,
    { workDir, expectedRevision, expectedStateRevision },
  );
  return data.plan;
}

export async function reconcileWorkstreamPlanStage(
  workstreamId: string,
  planId: string,
  stageId: string,
  workDir: string,
  expectedRevision: number,
  expectedStateRevision: number,
  runId: string,
): Promise<WorkstreamPlan> {
  const data = await postJSON<{ plan: WorkstreamPlan }>(
    `/api/workstreams/${encodeURIComponent(workstreamId)}/plans/${encodeURIComponent(planId)}/stages/${encodeURIComponent(stageId)}/reconcile`,
    { workDir, expectedRevision, expectedStateRevision, runId, manualAcknowledged: true },
  );
  return data.plan;
}

export async function listWorkstreams(workDir?: string): Promise<Workstream[]> {
  const params = workDir ? `?workDir=${encodeURIComponent(workDir)}` : '';
  const data = await fetchJSON<{ workstreams: Workstream[] }>(`/api/workstreams${params}`);
  return data.workstreams ?? [];
}

export async function createWorkstream(body: {
  workDir?: string;
  title: string;
  summary?: string;
  nextAction?: string;
  tags?: string[];
  goal?: Goal;
}): Promise<Workstream> {
  const data = await postJSON<{ workstream: Workstream }>('/api/workstreams', body);
  return data.workstream;
}

export async function generateHandoff(id: string, workDir?: string, planId?: string): Promise<{ path: string; markdown: string }> {
  return postJSON(`/api/workstreams/${encodeURIComponent(id)}/handoff`, {
    workDir,
    planId,
    includeReceipts: true,
    includeMemoryIndex: true,
  });
}
