import { useState } from 'react'
import { App, Button, Card, Input, List, Popconfirm, Space, Tag } from 'antd'
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons'

import { createGroup, deleteGroup, type GroupView } from '../../../api/watchlist'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import type { AsyncState } from '../../../hooks/useAsyncData'

interface Props {
  groups: AsyncState<GroupView[]>
  currentId: number | null
  onSelect: (id: number) => void
  /** 当前选中的分组被删掉时通知父级清空选择，否则会停在一个不存在的分组上。 */
  onDeleted: (id: number) => void
}

export function GroupSidebar({ groups, currentId, onSelect, onDeleted }: Props) {
  const { message } = App.useApp()
  const [newGroup, setNewGroup] = useState('')

  const onCreate = async () => {
    const name = newGroup.trim()
    if (!name) return
    try {
      await createGroup(name)
      setNewGroup('')
      groups.reload()
      message.success('分组已创建')
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '创建失败')
    }
  }

  const onDelete = async (id: number) => {
    try {
      await deleteGroup(id)
      onDeleted(id)
      groups.reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '删除失败')
    }
  }

  return (
    <Card title="分组" size="small">
      <AsyncBoundary
        loading={groups.loading}
        error={groups.error}
        onRetry={groups.reload}
        isEmpty={!!groups.data && groups.data.length === 0}
        emptyText="还没有分组"
      >
        <List
          size="small"
          dataSource={groups.data ?? []}
          renderItem={(g) => (
            <List.Item
              actions={[
                <Popconfirm
                  key="del"
                  title="删除这个分组？"
                  description="分组里的自选股会一并移除。"
                  onConfirm={() => onDelete(g.id!)}
                >
                  <Button
                    size="small"
                    type="text"
                    danger
                    icon={<DeleteOutlined />}
                    aria-label={`删除分组 ${g.name}`}
                  />
                </Popconfirm>,
              ]}
            >
              <Button
                type={currentId === g.id ? 'link' : 'text'}
                onClick={() => onSelect(g.id!)}
                style={{ paddingLeft: 0 }}
              >
                {g.name} <Tag>{g.itemCount ?? 0}</Tag>
              </Button>
            </List.Item>
          )}
        />
      </AsyncBoundary>

      <Space.Compact style={{ width: '100%', marginTop: 12 }}>
        <Input
          aria-label="新分组名称"
          placeholder="新分组名称"
          value={newGroup}
          onChange={(e) => setNewGroup(e.target.value)}
          onPressEnter={onCreate}
        />
        <Button icon={<PlusOutlined />} onClick={onCreate} aria-label="创建分组" />
      </Space.Compact>
    </Card>
  )
}
