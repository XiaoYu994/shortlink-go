import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authKeys, type Role } from '@/features/auth/api';
import { apiRequest } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';

export type InvitationStatus = 'pending' | 'accepted' | 'revoked' | 'expired';

export interface Member {
  user_id: number;
  email: string;
  name: string;
  role: Role;
  joined_at: string;
}

export interface Invitation {
  id: number;
  email: string;
  role: Role;
  status: InvitationStatus;
  invited_by?: number;
  created_at: string;
  expires_at?: string;
  accepted_at?: string;
  revoked_at?: string;
}

export const memberKeys = {
  members: (orgId: number) => ['orgs', orgId, 'members'] as const,
  invitations: (orgId: number) => ['orgs', orgId, 'invitations'] as const,
};

export function useMembers(orgId: number, enabled = true) {
  return useQuery({
    queryKey: memberKeys.members(orgId),
    queryFn: async ({ signal }) => {
      const res = await apiRequest<{ items: Member[] }>(ENDPOINTS.members.list, { orgId, signal });
      return res.items;
    },
    enabled,
  });
}

export function useUpdateMemberRole(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ userId, role }: { userId: number; role: Role }) =>
      apiRequest<Member>(ENDPOINTS.members.detail(userId), {
        method: 'PATCH',
        orgId,
        body: { role },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.members(orgId) });
      // 若修改的是当前用户自己的角色，刷新 /me 让侧边栏与按钮权限同步更新。
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}

export function useRemoveMember(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userId: number) =>
      apiRequest<void>(ENDPOINTS.members.detail(userId), {
        method: 'DELETE',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.members(orgId) });
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}

export function useInvitations(orgId: number) {
  return useQuery({
    queryKey: memberKeys.invitations(orgId),
    queryFn: async ({ signal }) => {
      const res = await apiRequest<{ items: Invitation[] }>(ENDPOINTS.invitations.listCreate, {
        orgId,
        signal,
      });
      return res.items;
    },
  });
}

export function useCreateInvitation(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (payload: { email: string; role: Role; expires_at?: string }) =>
      apiRequest<Invitation>(ENDPOINTS.invitations.listCreate, {
        method: 'POST',
        orgId,
        body: payload,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.invitations(orgId) });
    },
  });
}

export function useRevokeInvitation(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<void>(ENDPOINTS.invitations.detail(id), {
        method: 'DELETE',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: memberKeys.invitations(orgId) });
    },
  });
}
